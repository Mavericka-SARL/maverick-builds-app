package gateway

// Model export/import: a model at one specific revision serialized to a
// self-contained JSON package, and the reverse — recreating a model from
// such a package under a new application with every cross-entity reference
// remapped to freshly generated IDs. The entity graph and remapping logic
// live in internal/modeltransfer; this file is HTTP framing only: auth,
// tenant-scope checks, and audit logging.
//
// Both endpoints are restricted to the tenant_admin role — see the route
// registrations in NewHandler.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
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
	if pkg.ApprovedLocks, err = h.approvedLocks(ctx, modelID); err != nil {
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
	if req.Package.Version < modeltransfer.OldestPackageVersion || req.Package.Version > modeltransfer.PackageVersion {
		jsonErr(w, fmt.Errorf("unsupported package version %d (supported: %d to %d)", req.Package.Version, modeltransfer.OldestPackageVersion, modeltransfer.PackageVersion), http.StatusBadRequest)
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
			h.plans.CheckStorage(ctx, db, cid),
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

	notes := req.Package.StripUnreadWidgetProps()
	// What the import leaves out — a link to a row the package does not
	// hold, a member's parent in an unrelated dimension — is named, so a
	// package that imports with less than it says is told so.
	notes = append(notes, req.Package.DroppedReferences()...)
	modelID, revisionID, err := modeltransfer.Import(ctx, tx, req, act.UserID)
	if err != nil {
		jsonErr(w, fmt.Errorf("import failed: %w", err), http.StatusBadRequest)
		return
	}
	// The approvals that locked values in the exported workspace lock them
	// here too: each is made again as an approved run of its workflow.
	carried, err := carryApprovedLocks(ctx, tx, req.ApplicationID, act.UserID, req.Package.ApprovedLocks)
	if err != nil {
		jsonErr(w, fmt.Errorf("import failed: %w", err), http.StatusInternalServerError)
		return
	}
	notes = append(notes, req.Package.LockNotes(carried)...)
	if err := tx.Commit(ctx); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	// The package carries input facts but no calc results (nor does Import
	// write any); every calc metric in the imported model rendered blank in
	// every grid and dashboard until a user happened to edit a cell (found
	// live, 2026-09-10). Nor does it carry the records' postings.
	h.settleRevision(ctx, modelID, revisionID, act.UserID)

	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventModelImported,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: req.ApplicationID,
		ResourceType:  "model", ResourceID: modelID, RevisionID: revisionID,
		Metadata: map[string]string{"application_id": req.ApplicationID, "revision": req.Package.RevisionName},
	})
	out := map[string]any{"model_id": modelID, "revision_id": revisionID}
	if len(notes) > 0 {
		out["notes"] = notes
	}
	jsonOK(w, out)
}

// settleRevision completes a revision made wholesale — imported from a
// package or copied from another revision: its tables' postings rebuilt
// from the records (it arrives without them, and the first record saved
// there used to rebuild its table's totals from that one record alone,
// found 2026-10-18), then every calculated metric recalculated. Failures
// are logged: the revision itself is made.
func (h *handler) settleRevision(ctx context.Context, modelID, revisionID, userID string) {
	if _, err := h.records().RebuildPostings(ctx, modelID, revisionID, userID); err != nil {
		h.log.Warn().Err(err).Str("revision_id", revisionID).Msg("rebuilding the records' postings of a new revision failed")
	}
	if err := h.recalcRevisionFromInputs(ctx, modelID, revisionID); err != nil {
		h.log.Warn().Err(err).Str("revision_id", revisionID).Msg("recalc of a new revision failed")
	}
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
	sched := calculation.NewScheduler(h.log, calculation.NewStore(h.db.For(ctx)))
	return sched.RecalcFromAllInputs(ctx, modelID, revisionID, inputIDs)
}

// recalcRevisionCalculated recomputes every calculated metric of one
// revision, in dependency order — the whole-revision case of
// RecalcSpecific, for a revision whose members, properties and formulas
// changed where no recalculation followed (a revision being activated).
// Unlike recalcRevisionFromInputs it also reaches metrics that read only
// dimensions or dependency-free metrics.
func (h *handler) recalcRevisionCalculated(ctx context.Context, modelID, revisionID string) {
	ctx, done := h.backgroundRecalc(ctx, "recalculation of a revision")
	defer done()
	seq := h.requestRecalc(ctx, revisionID)
	defer h.recalcDone(ctx, revisionID, seq)
	h.recalcCalculated(ctx, modelID, revisionID)
}

// recalcRevisionInBackground is recalcRevisionCalculated after the request
// has answered, recorded as pending before it does: from the first read on,
// the grid shows calculated cells, and the KPI tiles their values, as about
// to change. An activation's recalculation used to run unrecorded, and the
// tiles showed half-computed figures (EBITDA equal to gross profit) as final.
func (h *handler) recalcRevisionInBackground(ctx context.Context, modelID, revisionID string) {
	h.recalcInBackground(ctx, revisionID, "recalculation of a revision", func(bg context.Context) {
		h.recalcCalculated(bg, modelID, revisionID)
	})
}

// recalcCalculated recomputes every calculated metric of the revision.
func (h *handler) recalcCalculated(ctx context.Context, modelID, revisionID string) {
	rows, err := h.db.Query(ctx,
		`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND NOT is_input`,
		modelID, revisionID)
	if err != nil {
		h.log.Warn().Err(err).Str("revision", revisionID).Msg("recalc revision: load metrics")
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	sched := calculation.NewScheduler(h.log, calculation.NewStore(h.db.For(ctx)))
	if err := sched.RecalcSpecific(ctx, modelID, revisionID, ids); err != nil {
		h.log.Warn().Err(err).Str("revision", revisionID).Msg("recalc revision failed")
	}
}

// approvedLocks lists the finished runs of modelID's application whose last
// approval approved and whose context names a member to lock (a budget
// year): the locks an import of the model does not carry. A run without a
// member in its context locks nothing (writeguard).
func (h *handler) approvedLocks(ctx context.Context, modelID string) ([]modeltransfer.ApprovedLock, error) {
	db := h.db.For(ctx)
	rows, err := db.Query(ctx, `
		SELECT wd.name, COALESCE(wi.context, '{}'::jsonb), COALESCE(wd.context_schema, '[]'::jsonb),
		       COALESCE(wi.completed_at, wi.started_at),
		       COALESCE((SELECT s.step_def_id FROM workflow.workflow_step s
		                 WHERE s.instance_id = wi.id AND s.decision = 'approve' AND s.completed_at IS NOT NULL
		                 ORDER BY s.completed_at DESC LIMIT 1), '')
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
	`+writeguard.LastApprovalJoinSQL+`
		WHERE wd.application_id = (SELECT application_id FROM core.model WHERE id = $1::uuid)
		  AND NOT COALESCE(wi.test_run, false) AND `+writeguard.ReopenableSQL+`
		ORDER BY wi.completed_at, wi.started_at`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []modeltransfer.ApprovedLock
	seen := map[string]bool{}
	var metricIDs []string
	for rows.Next() {
		var name, step string
		var ctxJSON, schemaJSON []byte
		var at time.Time
		if err := rows.Scan(&name, &ctxJSON, &schemaJSON, &at, &step); err != nil {
			return nil, err
		}
		var values map[string]any
		_ = json.Unmarshal(ctxJSON, &values)
		var vars []struct {
			Key      string `json:"key"`
			Label    string `json:"label"`
			DataType string `json:"data_type"`
		}
		_ = json.Unmarshal(schemaJSON, &vars)
		lock := modeltransfer.ApprovedLock{Workflow: name, ApprovedAt: at.UTC(), Context: map[string]string{}, ApprovalStep: step}
		members := 0
		for _, v := range vars {
			val := strings.TrimSpace(fmt.Sprint(values[v.Key]))
			if values[v.Key] == nil || val == "" || (v.DataType != "Dimension member" && v.DataType != "Metric") {
				continue
			}
			if v.DataType == "Dimension member" {
				members++
			} else if _, err := uuid.Parse(val); err == nil {
				metricIDs = append(metricIDs, val)
			}
			label := v.Label
			if label == "" {
				label = v.Key
			}
			lock.Scope = append(lock.Scope, label+" = "+val)
			lock.Context[v.Key] = val
		}
		if members == 0 {
			continue
		}
		key := name + "\x1f" + strings.Join(lock.Scope, "\x1f")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, lock)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A Metric field may hold a metric's id: carry its name, which the
	// imported model shares (a lock matches metrics by name).
	if len(metricIDs) > 0 {
		names := map[string]string{}
		nr, err := db.Query(ctx, `SELECT id::text, name FROM model.metric_def WHERE id = ANY($1::uuid[])`, metricIDs)
		if err != nil {
			return nil, err
		}
		for nr.Next() {
			var id, n string
			if nr.Scan(&id, &n) == nil {
				names[id] = n
			}
		}
		nr.Close()
		for i := range out {
			for k, v := range out[i].Context {
				if n, ok := names[v]; ok {
					out[i].Context[k] = n
					for j, sc := range out[i].Scope {
						out[i].Scope[j] = strings.Replace(sc, " = "+v, " = "+n, 1)
					}
				}
			}
		}
	}
	return out, nil
}

// carryApprovedLocks makes, in tx, each approval of a package's
// ApprovedLocks again as a finished run of the imported workflow of that
// name in appID, approved at its approval step: a lock is an approved run
// (writeguard), so the values it locked stay locked in the copy. carried[i]
// says whether ApprovedLocks[i] was made; a workflow the package did not
// bring, or one without that approval step, is left out and named.
func carryApprovedLocks(ctx context.Context, tx pgx.Tx, appID, userID string, locks []modeltransfer.ApprovedLock) ([]bool, error) {
	carried := make([]bool, len(locks))
	for i, l := range locks {
		if len(l.Context) == 0 {
			continue
		}
		var defID string
		var steps []byte
		err := tx.QueryRow(ctx, `
			SELECT id::text, COALESCE(steps, '[]'::jsonb) FROM workflow.workflow_def
			WHERE application_id = $1::uuid AND lower(name) = lower($2) AND status = 'published'
			ORDER BY (revision_id IS NOT NULL) DESC, created_at DESC LIMIT 1`, appID, l.Workflow).Scan(&defID, &steps)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return nil, err
		}
		step := l.ApprovalStep
		var defs []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		_ = json.Unmarshal(steps, &defs)
		ok := false
		for _, d := range defs {
			isApproval := strings.EqualFold(d.Type, "approval") || strings.EqualFold(d.Type, "step_type_approval") || d.Type == "2"
			if isApproval && (step == "" || d.ID == step) {
				step, ok = d.ID, true
				break
			}
		}
		if !ok {
			continue
		}
		ctxJSON, _ := json.Marshal(l.Context)
		at := l.ApprovedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		var instID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO workflow.workflow_instance (workflow_def_id, status, started_by, context, started_at, completed_at, steps_snapshot)
			VALUES ($1::uuid, 'completed', NULLIF($2, '')::uuid, $3::jsonb, $4, $4, $5::jsonb) RETURNING id::text`,
			defID, userID, ctxJSON, at, steps).Scan(&instID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, decision, comment, created_at, completed_at)
			VALUES ($1::uuid, $2, 'completed', 'approve', $3, $4, $4)`,
			instID, step, "Approved in the workspace the model was exported from; carried by the model import.", at); err != nil {
			return nil, err
		}
		carried[i] = true
	}
	return carried, nil
}
