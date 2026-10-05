package gateway

// Data exports: "file_export" integrations (model.integration_def) whose
// config is a dataexport.Spec — a grid's values written as CSV, XLSX or JSON
// in the layout, columns and number format someone specified.
//
//   POST  /api/developer/integrations {type:"file_export"}   create (spec validated)
//   PATCH /api/developer/integrations/{id}/config            change the spec (validated)
//   POST  /api/developer/integrations/export-preview         render a spec without saving
//   GET   /api/integrations/{id}/export                      download (any role with access)
//
// A download is built fresh for the person downloading it, from their own
// /api/grid view of the grid: hidden members and metrics never reach the
// file, and every value is the one their grid shows.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/dataexport"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// statusError carries the HTTP status of a failure from an in-process call.
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

func errStatus(err error, fallback int) int {
	var se *statusError
	if errors.As(err, &se) {
		return se.status
	}
	return fallback
}

// gridSnapshot is the requesting user's own /api/grid view of a grid, as a
// dataexport.Snapshot. It runs the grid handler in-process instead of
// re-deriving values: /api/grid is where input aggregation, calculated
// cells, recomputation for users with hidden members and withheld values
// are decided (a thousand lines of it), and an export that disagreed with
// the grid its downloader sees would be a second source of truth. The
// cloned request keeps the caller's identity, tenant and app headers.
func (h *handler) gridSnapshot(r *http.Request, gridID, revisionID string) (dataexport.Snapshot, error) {
	q := url.Values{"grid_def_id": {gridID}}
	if revisionID != "" {
		q.Set("revision_id", revisionID)
	}
	req := r.Clone(r.Context())
	req.Method = http.MethodGet
	req.URL = &url.URL{Path: "/api/grid", RawQuery: q.Encode()}
	req.RequestURI = ""
	req.Body = http.NoBody
	req.ContentLength = 0
	rec := httptest.NewRecorder()
	h.grid(rec, req)
	if rec.Code != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if e.Error == "" {
			e.Error = http.StatusText(rec.Code)
		}
		return dataexport.Snapshot{}, &statusError{status: rec.Code, msg: "read grid: " + e.Error}
	}
	var resp gridResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return dataexport.Snapshot{}, fmt.Errorf("read grid: %w", err)
	}
	if resp.RollupSourceGridID != nil {
		return dataexport.Snapshot{}, &statusError{status: http.StatusBadRequest,
			msg: "this grid mirrors another grid through a cross-dimension rollup and has no values of its own — export the source grid"}
	}

	snap := dataexport.Snapshot{Cells: resp.Cells, Texts: resp.Texts}
	for _, d := range resp.Dimensions {
		dim := dataexport.Dimension{ID: d.ID, Name: d.Name}
		for _, m := range d.Members {
			dim.Members = append(dim.Members, dataexport.Member{Code: m.Code, Label: m.Label, ParentCode: m.ParentCode})
		}
		snap.Dimensions = append(snap.Dimensions, dim)
	}
	// all_metrics carries each metric's own cell-key dimensions; metrics
	// (the grid's, in grid order) does not on this call shape.
	ownDims := map[string][]string{}
	for _, m := range resp.AllMetrics {
		ownDims[m.ID] = m.DimensionIDs
	}
	for _, m := range resp.Metrics {
		em := dataexport.Metric{ID: m.ID, Name: m.Name, Label: m.Label, DimensionIDs: ownDims[m.ID],
			Text: m.Format == metricformula.FormatText, Date: m.Format == metricformula.FormatDate}
		if len(m.PicklistOptions) > 0 {
			em.Picklist = make(map[float64]string, len(m.PicklistOptions))
			for _, o := range m.PicklistOptions {
				em.Picklist[o.Key] = o.Label
			}
		}
		snap.Metrics = append(snap.Metrics, em)
	}
	return snap, nil
}

// renderExport renders a spec against the requester's view of a grid.
func (h *handler) renderExport(r *http.Request, gridID, revisionID string, spec dataexport.Spec) (*dataexport.Table, error) {
	snap, err := h.gridSnapshot(r, gridID, revisionID)
	if err != nil {
		return nil, err
	}
	t, err := dataexport.Render(spec, snap)
	if err != nil {
		return nil, &statusError{status: http.StatusBadRequest, msg: err.Error()}
	}
	return t, nil
}

// writeSpecProblems answers 400 with every problem a spec has, so an
// editor can show them all at once.
func writeSpecProblems(w http.ResponseWriter, problems []string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":    "export spec: " + strings.Join(problems, "; "),
		"problems": problems,
	})
}

// exportGridInModel loads the export's grid and confirms it is this
// model's — and, when a revision is given, that revision's.
func (h *handler) exportGridInModel(r *http.Request, modelID, revisionID, gridID string) (dataexport.Grid, string, error) {
	ctx := r.Context()
	if err := h.checkIntegrationTarget(ctx, modelID, "grid", gridID); err != nil {
		return dataexport.Grid{}, "", err
	}
	g, _, gridRev, err := dataexport.LoadGrid(ctx, h.db.For(ctx), gridID)
	if err != nil {
		return dataexport.Grid{}, "", err
	}
	if revisionID != "" && gridRev != "" && gridRev != revisionID {
		return dataexport.Grid{}, "", fmt.Errorf("grid %q belongs to another revision — pick the grid of the revision you are working in", g.Name)
	}
	return g, gridRev, nil
}

func (h *handler) fileExportCreate(w http.ResponseWriter, r *http.Request, modelID, revisionID string, raw []byte) {
	ctx := r.Context()
	var body struct {
		Name       string          `json:"name"`
		TargetType string          `json:"target_type"`
		TargetID   string          `json:"target_id"`
		Status     string          `json:"status"`
		Tags       []string        `json:"tags"`
		Config     dataexport.Spec `json:"config"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body: %w", err), http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
		return
	}
	if body.TargetType != "" && body.TargetType != "grid" {
		jsonErr(w, fmt.Errorf("an export's target is a grid"), http.StatusBadRequest)
		return
	}
	if body.Status != "draft" {
		body.Status = "active"
	}
	if body.Tags == nil {
		body.Tags = []string{}
	}
	g, _, err := h.exportGridInModel(r, modelID, revisionID, body.TargetID)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	// A draft may be incomplete; an active export must be downloadable.
	if problems := dataexport.Validate(body.Config, g); len(problems) > 0 && body.Status == "active" {
		writeSpecProblems(w, problems)
		return
	}
	cfg, _ := json.Marshal(body.Config)
	var newID string
	if err := h.db.QueryRow(ctx, `
		INSERT INTO model.integration_def (model_id, name, type, target_type, target_id, revision_id, status, tags, config)
		VALUES ($1::uuid, $2, 'file_export', 'grid', $3::uuid, NULLIF($4,'')::uuid, $5, $6, $7::jsonb) RETURNING id::text`,
		modelID, body.Name, body.TargetID, revisionID, body.Status, body.Tags, string(cfg),
	).Scan(&newID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if a, e := h.resolveActor(ctx, r); e == nil {
		var appID string
		_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventIntegrationCreated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: appID, ResourceType: "integration", ResourceID: newID, RevisionID: revisionID,
			Metadata: map[string]string{"name": body.Name, "type": "file_export"},
		})
	}
	jsonOK(w, map[string]string{"id": newID, "status": "created"})
}

// fileExportConfigPatch replaces an export's spec, validated against its
// grid — unless the export is a draft, which may be saved incomplete.
func (h *handler) fileExportConfigPatch(w http.ResponseWriter, r *http.Request, intID string) {
	ctx := r.Context()
	var body struct {
		Config dataexport.Spec `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("config required: %w", err), http.StatusBadRequest)
		return
	}
	var modelID, gridID, status, revisionID string
	if err := h.db.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(target_id::text,''), status, COALESCE(revision_id::text,'')
		FROM model.integration_def WHERE id=$1::uuid`, intID).Scan(&modelID, &gridID, &status, &revisionID); err != nil {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	if status == "active" {
		g, _, err := h.exportGridInModel(r, modelID, "", gridID)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		if problems := dataexport.Validate(body.Config, g); len(problems) > 0 {
			writeSpecProblems(w, problems)
			return
		}
	}
	cfg, _ := json.Marshal(body.Config)
	if _, err := h.db.Exec(ctx, `UPDATE model.integration_def SET config=$2::jsonb WHERE id=$1::uuid`, intID, string(cfg)); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if a, e := h.resolveActor(ctx, r); e == nil {
		_, appID := h.integrationScope(ctx, intID)
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventIntegrationUpdated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: appID, ResourceType: "integration", ResourceID: intID, RevisionID: revisionID,
			Metadata: map[string]string{"sub_action": "config"},
		})
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// fileExportCheckRetarget refuses moving an active export onto a grid its
// spec does not fit (the generic PATCH /api/developer/integrations/{id}).
func (h *handler) fileExportCheckRetarget(r *http.Request, intID, targetType, targetID string, activating bool) error {
	ctx := r.Context()
	var modelID, storedTarget, status string
	var cfg []byte
	if err := h.db.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(target_id::text,''), status, COALESCE(config,'{}'::jsonb)
		FROM model.integration_def WHERE id=$1::uuid`, intID).Scan(&modelID, &storedTarget, &status, &cfg); err != nil {
		return fmt.Errorf("integration not found")
	}
	if targetType != "grid" {
		return fmt.Errorf("an export's target is a grid")
	}
	if targetID == storedTarget && !activating {
		return nil
	}
	if status != "active" && !activating {
		return nil
	}
	g, _, err := h.exportGridInModel(r, modelID, "", targetID)
	if err != nil {
		return err
	}
	var spec dataexport.Spec
	_ = json.Unmarshal(cfg, &spec)
	return dataexport.ValidationError(spec, g)
}

// exportPreview renders a spec against a grid without saving anything:
// the console editor's live preview, and the AI Developer's preview_export.
func (h *handler) exportPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var body struct {
		TargetID string          `json:"target_id"`
		Name     string          `json:"name"`
		Config   dataexport.Spec `json:"config"`
		Rows     int             `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TargetID == "" {
		jsonErr(w, fmt.Errorf("target_id (the grid) and config are required"), http.StatusBadRequest)
		return
	}
	// The grid names its own model (an app may have several); its
	// structure is read only once the caller may open that model.
	var gridModel string
	if err := h.db.QueryRow(ctx, `SELECT model_id::text FROM model.grid_def WHERE id = CASE WHEN $1 ~* '^[0-9a-f-]{36}$' THEN $1::uuid END`,
		body.TargetID).Scan(&gridModel); err != nil {
		jsonErr(w, fmt.Errorf("grid not found"), http.StatusNotFound)
		return
	}
	if ok, aErr := h.actorCanAccessModel(ctx, act, gridModel); aErr != nil {
		jsonErr(w, aErr, http.StatusInternalServerError)
		return
	} else if !ok {
		jsonErr(w, fmt.Errorf("grid not found"), http.StatusNotFound)
		return
	}
	g, _, gridRev, err := dataexport.LoadGrid(ctx, h.db.For(ctx), body.TargetID)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if problems := dataexport.Validate(body.Config, g); len(problems) > 0 {
		writeSpecProblems(w, problems)
		return
	}
	t, err := h.renderExport(r, body.TargetID, gridRev, body.Config)
	if err != nil {
		jsonErr(w, err, errStatus(err, http.StatusInternalServerError))
		return
	}
	n := body.Rows
	if n <= 0 || n > 200 {
		n = 20
	}
	name := body.Name
	if name == "" {
		name = g.Name
	}
	rows := t.TextRows(n)
	if rows == nil {
		rows = [][]string{}
	}
	jsonOK(w, map[string]any{
		"header":         t.Header,
		"default_header": t.DefaultHeader,
		"rows":           rows,
		"total_rows":     len(t.Rows),
		"warnings":       append([]string{}, t.Warnings...),
		"file_name":      dataexport.FileName(body.Config, name),
		"summary":        dataexport.Describe(body.Config),
	})
}

// integrationExport is GET /api/integrations/{id}/export: the file, built
// for whoever asks, from their own view of the grid.
func (h *handler) integrationExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	intID := r.PathValue("id")
	var name, typ, status, gridID, appID, revisionID string
	var cfg []byte
	if err := h.db.QueryRow(ctx, `
		SELECT i.name, i.type, i.status, COALESCE(i.target_id::text,''), m.application_id::text,
		       COALESCE(i.revision_id::text, m.active_revision_id::text, ''), COALESCE(i.config,'{}'::jsonb)
		FROM model.integration_def i JOIN core.model m ON m.id = i.model_id
		WHERE i.id = CASE WHEN $1 ~* '^[0-9a-f-]{36}$' THEN $1::uuid END`, intID,
	).Scan(&name, &typ, &status, &gridID, &appID, &revisionID, &cfg); err != nil {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	if ok, caErr := h.actorCanAccessApp(ctx, act, appID); caErr != nil {
		jsonErr(w, caErr, http.StatusInternalServerError)
		return
	} else if !ok {
		// Not found, not forbidden: an id outside your scope is not
		// confirmed to exist.
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	// An export reads its integration's revision: the open one, unless the
	// caller builds the model (revision_access.go).
	if !h.revisionOpen(ctx, act, h.revisionModel(ctx, revisionID), revisionID) {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	if typ != "file_export" {
		jsonErr(w, fmt.Errorf("integration %q imports data; only an export integration is downloaded", name), http.StatusBadRequest)
		return
	}
	if status == "draft" {
		jsonErr(w, fmt.Errorf("export %q is a draft — finish it in the Integrations tab first", name), http.StatusBadRequest)
		return
	}
	var spec dataexport.Spec
	if err := json.Unmarshal(cfg, &spec); err != nil {
		jsonErr(w, fmt.Errorf("export %q has an unreadable spec", name), http.StatusInternalServerError)
		return
	}
	t, err := h.renderExport(r, gridID, revisionID, spec)
	if err != nil {
		jsonErr(w, err, errStatus(err, http.StatusInternalServerError))
		return
	}
	var buf bytes.Buffer
	if err := dataexport.Write(&buf, t); err != nil {
		jsonErr(w, fmt.Errorf("write export: %w", err), http.StatusInternalServerError)
		return
	}
	h.recordIntegrationRun(ctx, intID, act.UserID, len(t.Rows), 0, "success", "exported "+strconv.Itoa(len(t.Rows))+" row(s) as "+t.Spec().Format)
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventIntegrationRun,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: appID, ResourceType: "integration", ResourceID: intID, RevisionID: revisionID,
		Metadata: map[string]string{"integration_type": "file_export", "rows": strconv.Itoa(len(t.Rows)), "format": t.Spec().Format},
	})
	w.Header().Set("Content-Type", dataexport.ContentType(t.Spec().Format))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", dataexport.FileName(spec, name)))
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}
