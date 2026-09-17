package gateway

// Model export/import: a model at one specific revision serialized to a
// self-contained JSON package, and the reverse — recreating a model from
// such a package under a new application with every cross-entity reference
// remapped to freshly generated IDs. The entity graph and remapping logic
// live in internal/modeltransfer (also used by internal/deployment's
// tarball builder — see that package's doc comment); this file is HTTP
// framing only: auth, tenant-scope checks, and audit logging.
//
// Both endpoints are restricted to the tenant_admin role — see the route
// registrations in NewHandler.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// ── Export ────────────────────────────────────────────────────────────────────

// adminModelExport serves GET /api/admin/models/{id}/export?revision_id=…
// (tenant_admin only). Defaults to the model's active revision.
func (h *handler) adminModelExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID := r.PathValue("id")

	// The route guard only checks the caller holds the tenant_admin role
	// somewhere — it says nothing about WHICH tenant. Without this check any
	// tenant_admin could export (read: exfiltrate) any other tenant's model
	// just by knowing its ID, since nothing else here is scoped to a tenant.
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

	revisionID, revisionName, err := modeltransfer.ResolveRevision(ctx, h.db.For(ctx), modelID, r.URL.Query().Get("revision_id"))
	if err != nil {
		jsonErr(w, fmt.Errorf("model has no revisions to export"), http.StatusBadRequest)
		return
	}

	includeData, err := parseIncludeData(r)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	pkg, err := modeltransfer.CollectExportWithOptions(ctx, h.db.For(ctx), modelID, revisionID, revisionName,
		modeltransfer.ExportOptions{IncludeData: includeData})
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventModelExported,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "model", ResourceID: modelID, RevisionID: revisionID,
		Metadata: map[string]string{"revision": revisionName, "include_data": strconv.FormatBool(includeData)},
	})

	suffix := ""
	if !includeData {
		suffix = "-definitions"
	}
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("%s-%s%s.mavericks-model.json", slugify(pkg.ModelName), slugify(revisionName), suffix)))
	jsonOK(w, pkg)
}

// parseIncludeData reads ?include_data= (default true): false exports the
// model's definitions without its entered values and form records.
func parseIncludeData(r *http.Request) (bool, error) {
	v := r.URL.Query().Get("include_data")
	if v == "" {
		return true, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("include_data must be true or false, got %q", v)
	}
	return b, nil
}

func slugify(s string) string {
	out := strings.ToLower(strings.TrimSpace(s))
	out = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, out)
	return strings.Trim(out, "-")
}

// ── Import ────────────────────────────────────────────────────────────────────

// adminModelImport serves POST /api/admin/models/import (tenant_admin only):
// recreates the packaged model under the given application as a new model
// with a single revision, remapping every cross-entity reference. Runs in
// one transaction — a failed import leaves nothing behind.
func (h *handler) adminModelImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	var req modeltransfer.ImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, fmt.Errorf("invalid body: %w", err), http.StatusBadRequest)
		return
	}
	if req.ApplicationID == "" {
		jsonErr(w, fmt.Errorf("application_id is required"), http.StatusBadRequest)
		return
	}
	if req.Package.Format != modeltransfer.PackageFormat {
		jsonErr(w, fmt.Errorf("unrecognized package format %q", req.Package.Format), http.StatusBadRequest)
		return
	}
	if req.Package.Version != modeltransfer.PackageVersion {
		jsonErr(w, fmt.Errorf("unsupported package version %d (supported: %d)", req.Package.Version, modeltransfer.PackageVersion), http.StatusBadRequest)
		return
	}
	// Same scope check as export, mirrored: the route guard only checks
	// "holds tenant_admin somewhere," not "owns this application" — without
	// this, any tenant_admin could write a model into any OTHER tenant's
	// application.
	if ok, scopeErr := h.adminCanAccessApp(ctx, act, req.ApplicationID); scopeErr != nil {
		jsonErr(w, scopeErr, http.StatusInternalServerError)
		return
	} else if !ok {
		jsonErr(w, fmt.Errorf("forbidden: application is outside your tenant scope"), http.StatusForbidden)
		return
	}

	// The plan's size limits apply to what the package would create: one
	// model, its metrics, its largest dimension and its data rows.
	if cid := h.customerOfApplication(ctx, req.ApplicationID); cid != "" && h.plans != nil {
		largest := 0
		for _, d := range req.Package.Dimensions {
			largest = max(largest, len(d.Members))
		}
		db := h.db.For(ctx)
		for _, check := range []error{
			h.plans.CheckModels(ctx, db, cid, 1),
			h.plans.CheckMetrics(ctx, db, cid, "", len(req.Package.Metrics)),
			h.plans.CheckMembers(ctx, db, cid, "", largest),
			h.plans.CheckFactRows(ctx, db, cid, "", len(req.Package.Facts)),
		} {
			if check != nil {
				h.jsonLimitErr(w, check)
				return
			}
		}
	}

	tx, err := h.db.Begin(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	modelID, revisionID, err := modeltransfer.Import(ctx, tx, req, act.UserID)
	if err != nil {
		jsonErr(w, fmt.Errorf("import failed: %w", err), http.StatusBadRequest)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	// The package carries input facts but no calc results (nor does Import
	// write any); every calc metric in the imported model rendered blank in
	// every grid and dashboard until a user happened to edit a cell (found
	// live, 2026-09-10). Same in-process scheduler call as cell writes.
	if err := h.recalcRevisionFromInputs(ctx, modelID, revisionID); err != nil {
		h.log.Warn().Err(err).Str("model_id", modelID).Msg("recalc after model import failed")
	}

	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventModelImported,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: req.ApplicationID,
		ResourceType:  "model", ResourceID: modelID, RevisionID: revisionID,
		Metadata: map[string]string{"application_id": req.ApplicationID, "revision": req.Package.RevisionName},
	})
	jsonOK(w, map[string]string{"model_id": modelID, "revision_id": revisionID})
}

// recalcRevisionFromInputs runs the calculation scheduler over every input
// metric of one revision, so a revision materialised wholesale — duplicated
// from another revision, or imported from a package — gets its calc_result
// rows immediately instead of showing blank calc metrics until the first
// cell edit. Every other write path (cells, form posting, CSV/Sheets import,
// dimension changes) already calls RecalcAffected the same way; this is the
// "all inputs changed" case of that.
func (h *handler) recalcRevisionFromInputs(ctx context.Context, modelID, revisionID string) error {
	rows, err := h.db.Query(ctx,
		`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND is_input`,
		modelID, revisionID)
	if err != nil {
		return err
	}
	var inputIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		inputIDs = append(inputIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(inputIDs) == 0 {
		return nil
	}
	sched := calculation.NewScheduler(h.log, calculation.NewStore(h.db.For(ctx)), nil)
	return sched.RecalcAffected(ctx, modelID, revisionID, inputIDs)
}
