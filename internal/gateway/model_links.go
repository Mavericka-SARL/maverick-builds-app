package gateway

// Model links: a connector of one model (integration_def type rest_api,
// Config protocol "model") whose source is a grid of another model of the
// same tenant. The rules, in one place:
//
//   - Only a developer of both models creates or changes a link's source,
//     activates it, switches its schedule on, tests it or runs it
//     (checkModelLink). Every run checks again that the developer it acts
//     for — the manual runner, or whoever switched the schedule on — still
//     builds both (ReadModelSource), so a developer who loses either model
//     stops the link rather than keeps reading through it.
//   - The source is read through /api/grid as that developer, in the source
//     model's active revision: the values that developer sees, calculated
//     ones included, and nothing a route would refuse them.
//   - Each side holds a switch. The link's own model holds its connector's
//     enabled flag (per revision copy, as for any connector); the source
//     model holds source_enabled, one switch for every copy of the link
//     (link_id), so a revision copied before a switch-off cannot bring it
//     back. A link runs only when both are on. A developer of either model
//     holds that model's switch; the tenant's administrators hold both.
//   - Both models belong to one tenant.
//
// The runs execute here, not in cmd/integration's worker, because the read
// is this process's own grid route (ModelLinks.Run).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/dataexport"
	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/internal/reporting"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// ModelLinks runs the model links of the databases it is started on. The
// gateway's handler fills it in (Deps.ModelLinks); cmd/gateway starts Run
// beside its other background loops, once per database.
type ModelLinks struct {
	h   *handler
	api http.Handler
}

// NewModelLinks returns a runner for Deps.ModelLinks.
func NewModelLinks() *ModelLinks { return &ModelLinks{} }

const (
	modelLinkClaimInterval = 2 * time.Second
	modelLinkLease         = 2 * time.Minute
	modelLinkRunTimeout    = 30 * time.Minute
	// modelLinkReadTimeout bounds the source read: a whole grid, which can
	// take longer than a chat connector's slice of one.
	modelLinkReadTimeout = 5 * time.Minute
)

// Run claims and runs pool's model links one at a time until ctx ends.
// customerID routes a dedicated tenant's database ("" in shared mode).
func (ml *ModelLinks) Run(ctx context.Context, pool *pgxpool.Pool, customerID string, log zerolog.Logger, workerID string) {
	if ml == nil || ml.h == nil {
		return
	}
	if customerID != "" {
		ctx = tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: customerID, Pool: pool})
	}
	runner := ml.runner(pool, log)
	for {
		ran, err := ml.runNext(ctx, runner, workerID)
		if err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("claim model link run")
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(modelLinkClaimInterval):
		}
	}
}

// runner is the integration runner of pool's model links.
func (ml *ModelLinks) runner(pool *pgxpool.Pool, log zerolog.Logger) *integration.Runner {
	return &integration.Runner{
		Store:         integration.NewStore(pool),
		Log:           log,
		AllowInsecure: ml.h.integrationAllowInsecure(),
		Committer:     &integration.DBCommitter{Pool: pool, Log: log},
		Source:        ml,
		OnFinished:    integration.DispatchRunEvents(pool, log),
	}
}

// runNext claims one queued model link run and runs it to completion,
// keeping its lease meanwhile. ran is false when there was none.
func (ml *ModelLinks) runNext(ctx context.Context, runner *integration.Runner, workerID string) (ran bool, err error) {
	store := runner.Store
	run, err := store.ClaimModelLink(ctx, workerID, modelLinkLease)
	if err != nil || run == nil {
		return false, err
	}
	runner.Log.Info().Str("run", run.ID).Str("integration", run.IntegrationID).
		Str("trigger", run.TriggerType).Bool("dry_run", run.DryRun).Msg("model link run claimed")
	runCtx, cancel := context.WithTimeout(ctx, modelLinkRunTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(modelLinkLease / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-runCtx.Done():
				return
			case <-t.C:
				_ = store.Heartbeat(runCtx, run.ID, workerID, modelLinkLease)
			}
		}
	}()
	runner.Execute(runCtx, run)
	close(done)
	return true, nil
}

// linkModels is what a link's two models say about it.
type linkModels struct {
	SourceAppID, SourceAppName, SourceName string
	ActiveRevisionID, ActiveRevisionName   string
	SameTenant                             bool
}

// loadLinkModels reads the source model of a link in targetModelID.
// pgx.ErrNoRows: either model does not exist.
func (h *handler) loadLinkModels(ctx context.Context, targetModelID, sourceModelID string) (linkModels, error) {
	var lm linkModels
	err := h.db.QueryRow(ctx, `
		SELECT sm.application_id::text, sapp.name, sm.name,
		       COALESCE(sm.active_revision_id::text, ''), COALESCE(r.name, ''),
		       sapp.customer_id IS NOT NULL AND sapp.customer_id = tapp.customer_id
		FROM core.model sm
		JOIN core.application sapp ON sapp.id = sm.application_id
		LEFT JOIN model.revision r ON r.id = sm.active_revision_id
		CROSS JOIN core.model tm
		JOIN core.application tapp ON tapp.id = tm.application_id
		WHERE sm.id = $1::uuid AND tm.id = $2::uuid
	`, sourceModelID, targetModelID).Scan(&lm.SourceAppID, &lm.SourceAppName, &lm.SourceName,
		&lm.ActiveRevisionID, &lm.ActiveRevisionName, &lm.SameTenant)
	return lm, err
}

// sourceGridID is the id of the grid named name in the source model's
// active revision ("" when there is none of that name).
func (h *handler) sourceGridID(ctx context.Context, modelID, revisionID, name string) (string, error) {
	var id string
	err := h.db.QueryRow(ctx, `
		SELECT id::text FROM model.grid_def
		WHERE model_id = $1::uuid AND revision_id = $2::uuid AND lower(name) = lower($3)
		ORDER BY created_at LIMIT 1
	`, modelID, revisionID, strings.TrimSpace(name)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// buildsModel reports whether a may build modelID: it holds the developer
// role and a developer's reach opens the model — the check every
// developer-only route makes of the model it works in.
func (h *handler) buildsModel(ctx context.Context, a *actor, modelID string) (bool, error) {
	if !a.hasRole("developer") {
		return false, nil
	}
	return h.actorCanAccessModel(withDeveloperRoute(ctx), a, modelID)
}

// errNotBothModels refuses a link change or run by someone who is not a
// developer of the model it reads.
var errNotBothModels = errors.New("only a developer of both models can set up, change, test or run a model link — you are not a developer of the model it reads")

// checkModelLink is the rule for saving or using a model link in modelID as
// a: no connection, and a source model of the same tenant, other than
// modelID, that a builds. When a grid is named it must be in the source
// model's active revision. A draft whose source is not chosen yet passes;
// strict (activation, a test or a run) wants one. Any other connector
// passes untouched.
func (h *handler) checkModelLink(ctx context.Context, a *actor, modelID string, cfg *integration.Config, connectionID string, strict bool) error {
	if cfg == nil || cfg.Protocol != integration.ProtocolModel {
		return nil
	}
	if connectionID != "" {
		return fmt.Errorf("a model link reads as the developer who runs it — it takes no connection")
	}
	if cfg.Model == nil || cfg.Model.ModelID == "" {
		if strict {
			return fmt.Errorf("choose the model the link reads")
		}
		return nil
	}
	src := cfg.Model
	if _, err := uuid.Parse(src.ModelID); err != nil {
		return fmt.Errorf("model.model_id must name the source model")
	}
	if src.ModelID == modelID {
		return fmt.Errorf("a model link reads another model — this model's own metrics are read with formulas")
	}
	lm, err := h.loadLinkModels(ctx, modelID, src.ModelID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !lm.SameTenant) {
		// The same answer for a model of another tenant and none at all.
		return fmt.Errorf("the source model is not a model of this tenant")
	}
	if err != nil {
		return err
	}
	if ok, berr := h.buildsModel(ctx, a, src.ModelID); berr != nil {
		return berr
	} else if !ok {
		return errNotBothModels
	}
	if strings.TrimSpace(src.Grid) == "" {
		if strict {
			return fmt.Errorf("choose the grid the link reads")
		}
		return nil
	}
	if lm.ActiveRevisionID == "" {
		return fmt.Errorf("model %q has no active revision to read", lm.SourceName)
	}
	gridID, err := h.sourceGridID(ctx, src.ModelID, lm.ActiveRevisionID, src.Grid)
	if err != nil {
		return err
	}
	if gridID == "" {
		return fmt.Errorf("model %q has no grid %q in its active revision %q", lm.SourceName, src.Grid, lm.ActiveRevisionName)
	}
	return nil
}

// checkModelLinkRequest is checkModelLink for the request's caller.
func (h *handler) checkModelLinkRequest(r *http.Request, modelID string, cfg *integration.Config, connectionID string, strict bool) error {
	if cfg == nil || cfg.Protocol != integration.ProtocolModel {
		return nil
	}
	a, err := h.resolveActor(r.Context(), r)
	if err != nil {
		return err
	}
	return h.checkModelLink(r.Context(), a, modelID, cfg, connectionID, strict)
}

// ReadModelSource implements integration.SourceReader: the source grid,
// read through /api/grid as runBy, after the checks a save makes are made
// again for runBy — the link's models may have changed, and runBy's access
// with them, since anyone looked.
func (ml *ModelLinks) ReadModelSource(ctx context.Context, def *integration.Definition, runBy string) (*integration.SourceTable, error) {
	h := ml.h
	src := def.Config.Model
	refuse := func(code, format string, args ...any) (*integration.SourceTable, error) {
		return nil, &integration.SourceError{Code: code, Msg: fmt.Sprintf(format, args...)}
	}
	lm, err := h.loadLinkModels(ctx, def.ModelID, src.ModelID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !lm.SameTenant) {
		return refuse(integration.ErrCodeInvalidData, "the source model no longer exists in this tenant")
	}
	if err != nil {
		return nil, err
	}
	if src.ModelID == def.ModelID {
		return refuse(integration.ErrCodeInvalidData, "a model link reads another model")
	}

	var sub, name string
	if err := h.db.QueryRow(ctx, `
		SELECT COALESCE(keycloak_sub, ''), COALESCE(NULLIF(display_name, ''), email)
		FROM identity.user WHERE id = $1::uuid
	`, runBy).Scan(&sub, &name); err != nil || sub == "" {
		return refuse(integration.ErrCodeAuth, "the developer this run acts for has no account here any more — a developer of both models must run it or switch its schedule on again")
	}
	a, err := h.actorByKeycloakSub(ctx, sub)
	if err != nil {
		return refuse(integration.ErrCodeAuth, "%s can no longer sign in — a developer of both models must run the link or switch its schedule on again", name)
	}
	for _, m := range []string{def.ModelID, src.ModelID} {
		ok, berr := h.buildsModel(ctx, a, m)
		if berr != nil {
			return nil, berr
		}
		if !ok {
			return refuse(integration.ErrCodeAuth, "%s is no longer a developer of both models — a developer of both must run the link or switch its schedule on again", name)
		}
	}

	if lm.ActiveRevisionID == "" {
		return refuse(integration.ErrCodeInvalidData, "model %q has no active revision to read", lm.SourceName)
	}
	gridID, err := h.sourceGridID(ctx, src.ModelID, lm.ActiveRevisionID, src.Grid)
	if err != nil {
		return nil, err
	}
	if gridID == "" {
		return refuse(integration.ErrCodeInvalidData, "model %q has no grid %q in its active revision %q", lm.SourceName, src.Grid, lm.ActiveRevisionName)
	}

	resp, err := mcpReader{api: ml.api, timeout: modelLinkReadTimeout}.Read(ctx, sub, reporting.Request{
		Method:  http.MethodGet,
		Path:    "/api/grid",
		Query:   url.Values{"grid_def_id": {gridID}, "revision_id": {lm.ActiveRevisionID}},
		AppID:   lm.SourceAppID,
		ModelID: src.ModelID,
	})
	if err != nil {
		return refuse(integration.ErrCodeTimeout, "read grid %q: %v", src.Grid, err)
	}
	snap, err := gridResponseSnapshot(resp.Status, resp.Body)
	if err != nil {
		code := integration.ErrCodeInvalidData
		if resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden || resp.Status == http.StatusNotFound {
			code = integration.ErrCodeAuth
		}
		return refuse(code, "%v", err)
	}
	display := src.MemberDisplay
	if display == "" {
		display = "code"
	}
	t, err := dataexport.Render(dataexport.Spec{Metrics: src.Metrics, Filters: src.Filters, MemberDisplay: display}, snap)
	if err != nil {
		return refuse(integration.ErrCodeInvalidData, "grid %q: %v", src.Grid, err)
	}
	return &integration.SourceTable{
		Header:   t.Header,
		Rows:     t.TextRows(0),
		Model:    lm.SourceAppName + " › " + lm.SourceName,
		Revision: lm.ActiveRevisionName,
		Warnings: t.Warnings,
	}, nil
}

// ── Listing ─────────────────────────────────────────────────────────────────

// modelLinkEnd is one side of a link.
type modelLinkEnd struct {
	ApplicationID   string `json:"application_id"`
	ApplicationName string `json:"application_name"`
	ModelID         string `json:"model_id"`
	ModelName       string `json:"model_name"`
	// Revision is the target side's revision copy, and whether it is that
	// model's active revision; Grid is the source grid's name.
	Revision       string `json:"revision,omitempty"`
	ActiveRevision bool   `json:"active_revision,omitempty"`
	Grid           string `json:"grid,omitempty"`
}

// modelLinkRun is a link's last real run.
type modelLinkRun struct {
	Status         string     `json:"status"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	ErrorCode      string     `json:"error_code,omitempty"`
	Message        string     `json:"message,omitempty"`
	RecordsWritten int        `json:"records_written"`
}

// modelLinkItem is one revision copy of a link, as the source side and
// the administrators list it.
type modelLinkItem struct {
	ID               string        `json:"id"`
	LinkID           string        `json:"link_id"`
	Name             string        `json:"name"`
	Status           string        `json:"status"`
	Enabled          bool          `json:"enabled"`
	SourceEnabled    bool          `json:"source_enabled"`
	SourceSwitchedBy string        `json:"source_switched_by,omitempty"`
	SourceSwitchedAt *time.Time    `json:"source_switched_at,omitempty"`
	Schedule         string        `json:"schedule"`
	ScheduleBy       string        `json:"schedule_by,omitempty"`
	Target           modelLinkEnd  `json:"target"`
	Source           modelLinkEnd  `json:"source"`
	LastRun          *modelLinkRun `json:"last_run,omitempty"`
}

// listModelLinks lists the model links matching where (a condition on i,
// the integration, tapp and sapp, the two applications), with args.
func (h *handler) listModelLinks(ctx context.Context, where string, args ...any) ([]modelLinkItem, error) {
	rows, err := h.db.Query(ctx, `
		SELECT i.id::text, i.link_id::text, i.name, i.status, i.enabled, i.source_enabled,
		       COALESCE(NULLIF(su.display_name, ''), su.email, ''), i.source_switched_at,
		       CASE WHEN sc.kind IS NULL OR sc.kind = 'manual' THEN 'manual'
		            WHEN sc.kind = 'interval' THEN 'every ' || sc.interval_seconds || 's'
		            ELSE sc.cron_expr || ' (' || sc.timezone || ')' END
		         || CASE WHEN sc.kind IS NOT NULL AND sc.kind <> 'manual' AND NOT sc.enabled THEN ' — off' ELSE '' END,
		       COALESCE(NULLIF(eu.display_name, ''), eu.email, ''),
		       tapp.id::text, tapp.name, tm.id::text, tm.name, COALESCE(rev.name, ''),
		       COALESCE(tm.active_revision_id = i.revision_id, false),
		       sapp.id::text, sapp.name, sm.id::text, sm.name, COALESCE(i.config->'model'->>'grid', ''),
		       lr.status, lr.finished_at, lr.error_code, lr.message, lr.records_written
		FROM model.integration_def i
		JOIN core.model tm ON tm.id = i.model_id
		JOIN core.application tapp ON tapp.id = tm.application_id
		JOIN core.model sm ON sm.id = i.source_model_id
		JOIN core.application sapp ON sapp.id = sm.application_id
		LEFT JOIN model.revision rev ON rev.id = i.revision_id
		LEFT JOIN identity.user su ON su.id = i.source_switched_by
		LEFT JOIN model.integration_schedule sc ON sc.integration_id = i.id
		LEFT JOIN identity.user eu ON eu.id = sc.enabled_by
		LEFT JOIN LATERAL (
		    SELECT status, finished_at, error_code, message, records_written
		    FROM model.integration_run
		    WHERE integration_id = i.id AND NOT dry_run AND trigger_type <> 'test'
		    ORDER BY created_at DESC LIMIT 1
		) lr ON true
		WHERE i.type = 'rest_api' AND `+where+`
		ORDER BY tapp.name, tm.name, i.name, rev.name
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []modelLinkItem{}
	for rows.Next() {
		var it modelLinkItem
		var runStatus, runCode, runMsg *string
		var runAt *time.Time
		var runWritten *int
		if err := rows.Scan(&it.ID, &it.LinkID, &it.Name, &it.Status, &it.Enabled, &it.SourceEnabled,
			&it.SourceSwitchedBy, &it.SourceSwitchedAt, &it.Schedule, &it.ScheduleBy,
			&it.Target.ApplicationID, &it.Target.ApplicationName, &it.Target.ModelID, &it.Target.ModelName,
			&it.Target.Revision, &it.Target.ActiveRevision,
			&it.Source.ApplicationID, &it.Source.ApplicationName, &it.Source.ModelID, &it.Source.ModelName, &it.Source.Grid,
			&runStatus, &runAt, &runCode, &runMsg, &runWritten); err != nil {
			return nil, err
		}
		if runStatus != nil {
			it.LastRun = &modelLinkRun{Status: *runStatus, FinishedAt: runAt}
			if runCode != nil {
				it.LastRun.ErrorCode = *runCode
			}
			if runMsg != nil {
				it.LastRun.Message = *runMsg
			}
			if runWritten != nil {
				it.LastRun.RecordsWritten = *runWritten
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// modelLinkRow is what a switch needs to know of a link.
type modelLinkRow struct {
	ID, ModelID, AppID, SourceModelID, LinkID, CustomerID string
}

func (h *handler) loadModelLinkRow(ctx context.Context, id string) (modelLinkRow, error) {
	var row modelLinkRow
	if _, err := uuid.Parse(id); err != nil {
		return row, pgx.ErrNoRows
	}
	err := h.db.QueryRow(ctx, `
		SELECT i.id::text, i.model_id::text, m.application_id::text, i.source_model_id::text,
		       i.link_id::text, COALESCE(app.customer_id::text, '')
		FROM model.integration_def i
		JOIN core.model m ON m.id = i.model_id
		JOIN core.application app ON app.id = m.application_id
		WHERE i.id = $1::uuid AND i.type = 'rest_api' AND i.source_model_id IS NOT NULL
	`, id).Scan(&row.ID, &row.ModelID, &row.AppID, &row.SourceModelID, &row.LinkID, &row.CustomerID)
	return row, err
}

// switchModelLink sets one side's switch of a link and records who did.
// The source side's switch is every revision copy's: link_id, for the same
// source model.
func (h *handler) switchModelLink(r *http.Request, a *actor, row modelLinkRow, side string, on bool) error {
	ctx := r.Context()
	var err error
	if side == "source" {
		_, err = h.db.Exec(ctx, `
			UPDATE model.integration_def
			SET source_enabled = $3, source_switched_by = $4::uuid, source_switched_at = now()
			WHERE link_id = $1::uuid AND source_model_id = $2::uuid
		`, row.LinkID, row.SourceModelID, on, a.UserID)
	} else {
		_, err = h.db.Exec(ctx, `UPDATE model.integration_def SET enabled = $2 WHERE id = $1::uuid`, row.ID, on)
	}
	if err != nil {
		return err
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventIntegrationSwitched,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ApplicationID: row.AppID, ResourceType: "integration", ResourceID: row.ID,
		Metadata: map[string]string{"side": side, "enabled": fmt.Sprint(on), "source_model_id": row.SourceModelID},
	})
	return nil
}

// ── Developer routes ────────────────────────────────────────────────────────

// developerModelLinks is GET /api/developer/model-links: the links that read
// the model the request works in, from any model of its tenant — the source
// side's view, with the switch it holds.
func (h *handler) developerModelLinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	links, err := h.listModelLinks(ctx, `i.source_model_id = $1::uuid`, modelID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, links)
}

// developerModelLinkSwitch is PATCH /api/developer/model-links/{id}
// {"source_enabled": bool}: the source side's switch, held by the source
// model's developers. The link's own side switches it with the connector's
// own PATCH (enabled).
func (h *handler) developerModelLinkSwitch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		SourceEnabled *bool `json:"source_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SourceEnabled == nil {
		jsonErr(w, fmt.Errorf("source_enabled is required"), http.StatusBadRequest)
		return
	}
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	row, err := h.loadModelLinkRow(ctx, r.PathValue("id"))
	if err != nil {
		jsonErr(w, fmt.Errorf("model link not found"), http.StatusNotFound)
		return
	}
	// Only the source model's developers hold this switch; to anyone else
	// the link is not there.
	if ok, berr := h.buildsModel(ctx, a, row.SourceModelID); berr != nil || !ok {
		jsonErr(w, fmt.Errorf("model link not found"), http.StatusNotFound)
		return
	}
	if err := h.switchModelLink(r, a, row, "source", *body.SourceEnabled); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"id": row.ID, "source_enabled": *body.SourceEnabled})
}

// modelLinkSource is a model a link may read, with its active revision's
// grids.
type modelLinkSource struct {
	ApplicationID   string                `json:"application_id"`
	ApplicationName string                `json:"application_name"`
	ModelID         string                `json:"model_id"`
	ModelName       string                `json:"model_name"`
	Revision        string                `json:"revision"`
	Grids           []modelLinkSourceGrid `json:"grids"`
}

type modelLinkSourceGrid struct {
	Name       string                  `json:"name"`
	Metrics    []modelLinkSourceMetric `json:"metrics"`
	Dimensions []modelLinkSourceDim    `json:"dimensions"`
}

type modelLinkSourceMetric struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
}

type modelLinkSourceDim struct {
	Name    string                  `json:"name"`
	Members []modelLinkSourceMember `json:"members,omitempty"`
}

type modelLinkSourceMember struct {
	Code       string `json:"code"`
	Label      string `json:"label"`
	ParentCode string `json:"parent_code,omitempty"`
}

// developerModelLinkSources is GET /api/developer/model-link-sources: the
// models a link in the model the request works in may read — the other
// models of its tenant the caller builds, each with its active revision's
// grids. ?model_id=…&grid=… answers one grid with its dimensions' members,
// for a link's filters.
func (h *handler) developerModelLinkSources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	onlyModel, onlyGrid := r.URL.Query().Get("model_id"), r.URL.Query().Get("grid")
	rows, err := h.db.Query(ctx, `
		SELECT sm.id::text, sm.name, sapp.id::text, sapp.name,
		       COALESCE(sm.active_revision_id::text, ''), COALESCE(rev.name, '')
		FROM core.model tm
		JOIN core.application tapp ON tapp.id = tm.application_id
		JOIN core.application sapp ON sapp.customer_id = tapp.customer_id
		JOIN core.model sm ON sm.application_id = sapp.id AND sm.id <> tm.id
		LEFT JOIN model.revision rev ON rev.id = sm.active_revision_id
		WHERE tm.id = $1::uuid AND ($2 = '' OR sm.id::text = $2)
		ORDER BY sapp.name, sm.name
	`, modelID, onlyModel)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	type cand struct {
		src   modelLinkSource
		revID string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.src.ModelID, &c.src.ModelName, &c.src.ApplicationID, &c.src.ApplicationName, &c.revID, &c.src.Revision); err != nil {
			rows.Close()
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		cands = append(cands, c)
	}
	rows.Close()
	out := []modelLinkSource{}
	for _, c := range cands {
		if ok, berr := h.buildsModel(ctx, a, c.src.ModelID); berr != nil || !ok {
			continue
		}
		c.src.Grids = []modelLinkSourceGrid{}
		if c.revID != "" {
			grids, gerr := h.modelLinkSourceGrids(ctx, c.src.ModelID, c.revID, onlyGrid)
			if gerr != nil {
				jsonErr(w, gerr, http.StatusInternalServerError)
				return
			}
			c.src.Grids = grids
		}
		out = append(out, c.src)
	}
	jsonOK(w, out)
}

// modelLinkSourceGrids lists a revision's grids with their metrics and
// dimensions; members only for the grid named withMembers.
func (h *handler) modelLinkSourceGrids(ctx context.Context, modelID, revisionID, withMembers string) ([]modelLinkSourceGrid, error) {
	rows, err := h.db.Query(ctx, `
		SELECT id::text, name FROM model.grid_def
		WHERE model_id = $1::uuid AND revision_id = $2::uuid AND rollup_source_grid_id IS NULL
		ORDER BY name
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	type gridRef struct{ id, name string }
	var refs []gridRef
	for rows.Next() {
		var g gridRef
		if err := rows.Scan(&g.id, &g.name); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, g)
	}
	rows.Close()
	out := []modelLinkSourceGrid{}
	for _, ref := range refs {
		if withMembers != "" && !strings.EqualFold(ref.name, withMembers) {
			continue
		}
		g, _, _, err := dataexport.LoadGrid(ctx, h.db.For(ctx), ref.id)
		if err != nil {
			return nil, err
		}
		item := modelLinkSourceGrid{Name: ref.name, Metrics: []modelLinkSourceMetric{}, Dimensions: []modelLinkSourceDim{}}
		for _, m := range g.Metrics {
			item.Metrics = append(item.Metrics, modelLinkSourceMetric{Name: m.Name, Label: m.Label})
		}
		for _, d := range g.Dimensions {
			dim := modelLinkSourceDim{Name: d.Name}
			if withMembers != "" {
				dim.Members = make([]modelLinkSourceMember, 0, len(d.Members))
				for _, m := range d.Members {
					dim.Members = append(dim.Members, modelLinkSourceMember{Code: m.Code, Label: m.Label, ParentCode: m.ParentCode})
				}
			}
			item.Dimensions = append(item.Dimensions, dim)
		}
		out = append(out, item)
	}
	return out, nil
}

// ── Administrator routes ────────────────────────────────────────────────────

// adminModelLinks is GET /api/admin/model-links: every model link of the
// tenants the caller administers, with both switches.
func (h *handler) adminModelLinks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	all, customerIDs, err := h.adminScopeCustomerIDs(ctx, a)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	var links []modelLinkItem
	if all {
		links, err = h.listModelLinks(ctx, `true`)
	} else {
		links, err = h.listModelLinks(ctx, `tapp.customer_id::text = ANY($1)`, customerIDs)
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, links)
}

// adminModelLinkSwitch is PATCH /api/admin/model-links/{id}
// {"enabled"?: bool, "source_enabled"?: bool}: a tenant administrator holds
// both sides' switches.
func (h *handler) adminModelLinkSwitch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Enabled       *bool `json:"enabled"`
		SourceEnabled *bool `json:"source_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Enabled == nil && body.SourceEnabled == nil) {
		jsonErr(w, fmt.Errorf("enabled or source_enabled is required"), http.StatusBadRequest)
		return
	}
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	row, err := h.loadModelLinkRow(ctx, r.PathValue("id"))
	if err != nil {
		jsonErr(w, fmt.Errorf("model link not found"), http.StatusNotFound)
		return
	}
	all, customerIDs, err := h.adminScopeCustomerIDs(ctx, a)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	inScope := all
	for _, c := range customerIDs {
		if c == row.CustomerID {
			inScope = true
		}
	}
	if !inScope {
		jsonErr(w, fmt.Errorf("model link not found"), http.StatusNotFound)
		return
	}
	if body.Enabled != nil {
		if err := h.switchModelLink(r, a, row, "target", *body.Enabled); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	if body.SourceEnabled != nil {
		if err := h.switchModelLink(r, a, row, "source", *body.SourceEnabled); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	jsonOK(w, map[string]string{"status": "ok"})
}
