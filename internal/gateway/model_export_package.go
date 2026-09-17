package gateway

// Standalone deployment package export: the tenant_admin-gated tar.gz
// counterpart to adminModelExport's JSON package (model_transfer.go).
// Delegates entity-graph collection to the exact same
// internal/modeltransfer code via internal/deployment.Builder — see that
// package's doc comment for what it repairs relative to the previous
// (unreachable, broken) gRPC implementation. Retention goes through
// pkg/objectstore (h.store) — nothing here touches a filesystem.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/deployment"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// adminModelExportPackage serves GET /api/admin/models/{id}/export/package
// ?revision_id=… (tenant_admin only). Defaults to the model's active
// revision. Returns a tar.gz containing the model's full entity graph
// (package.json), the complete migrations/ tree, a provenance manifest,
// and an infra-only docker-compose + README. The same bytes are also
// retained in object storage, keyed by revision — a re-export overwrites
// the previous one rather than accumulating history (see pkg/objectstore's
// MetaStore.Upsert).
func (h *handler) adminModelExportPackage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID := r.PathValue("id")

	// Same tenant-scope check as adminModelExport — the route guard only
	// checks the caller holds tenant_admin somewhere, not which tenant.
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	if ok, scopeErr := h.adminCanAccessModel(ctx, act, modelID); scopeErr != nil {
		jsonErr(w, scopeErr, http.StatusInternalServerError)
		return
	} else if !ok {
		jsonErr(w, fmt.Errorf("forbidden: model is outside your tenant scope"), http.StatusForbidden)
		return
	}

	if h.store == nil {
		jsonErr(w, fmt.Errorf("object storage is not configured"), http.StatusInternalServerError)
		return
	}

	var modelName string
	if err := h.db.QueryRow(ctx, `SELECT name FROM core.model WHERE id=$1::uuid`, modelID).Scan(&modelName); err != nil {
		jsonErr(w, fmt.Errorf("model not found"), http.StatusNotFound)
		return
	}
	revisionID, revisionName, err := modeltransfer.ResolveRevision(ctx, h.db.For(ctx), modelID, r.URL.Query().Get("revision_id"))
	if err != nil {
		jsonErr(w, fmt.Errorf("model has no revisions to export"), http.StatusBadRequest)
		return
	}

	builder := deployment.NewBuilder(h.db.For(ctx), h.log)
	includeData, err := parseIncludeData(r)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	data, resolvedRevID, err := builder.BuildWithOptions(ctx, modelID, revisionID, modeltransfer.ExportOptions{IncludeData: includeData})
	if err != nil {
		jsonErr(w, fmt.Errorf("build package: %w", err), http.StatusInternalServerError)
		return
	}

	// Owner is the revision, not the model: each revision keeps its own
	// durable, overwritten-on-re-export package (decision: "retained
	// artifacts" = current-per-revision, not an unbounded history).
	key := fmt.Sprintf("packages/%s/%s.tar.gz", modelID, resolvedRevID)
	rec, err := h.store.Save(ctx, "deployment_package", resolvedRevID, key, data, "application/gzip", act.UserID)
	if err != nil {
		jsonErr(w, fmt.Errorf("save package: %w", err), http.StatusInternalServerError)
		return
	}

	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventModelExported,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "model", ResourceID: modelID, RevisionID: revisionID,
		Metadata: map[string]string{"revision": revisionName, "format": "package", "object_id": rec.ID, "object_key": rec.ObjectKey},
	})

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("%s-%s.tar.gz", slugify(modelName), slugify(revisionName))))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
