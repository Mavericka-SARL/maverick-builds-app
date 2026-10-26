package gateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// duplicateRevision copies automation rules whole. A rule scoped to one
// connector (integration_completed/failed with source_integration_id) is
// scoped to the copy's connector — the copy used to leave it NULL, which the
// dispatcher reads as "any connector". A schedule rule carries its cron
// settings — without cron_expr the copy broke automation_rule_schedule_cron_chk
// and the whole duplication failed — and comes unarmed (next_fire_at NULL),
// as does a connector's schedule (migration 151 made connector names unique
// in a revision, so every connector maps to its copy by name; twins are
// refused).
func TestDuplicateRevisionCopiesRuleScopeAndSchedule(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	newConnector := func(name string) string {
		return q(`INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id, config)
			VALUES ($1::uuid, $2::uuid, $3, 'csv_import', 'grid', $4::uuid, '{}'::jsonb) RETURNING id::text`,
			f.modelID, f.workingRevID, name, f.gridStaffID)
	}
	feed := newConnector("Feed")
	newConnector("Other")
	twin := newConnector("Twin")
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, config)
		VALUES ($1::uuid, $2::uuid, 'Twin', 'csv_import', 'grid', '{}'::jsonb)`, f.modelID, f.workingRevID); err == nil {
		t.Error("a second connector named Twin in one revision was accepted")
	}
	q(`INSERT INTO model.integration_schedule (integration_id, kind, interval_seconds, timezone, enabled, next_fire_at)
		VALUES ($1::uuid, 'interval', 3600, 'UTC', true, now()) RETURNING integration_id::text`, feed)
	for name, integ := range map[string]string{"OnFeed": feed, "OnTwin": twin} {
		q(`INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, revision_id, source_integration_id)
			VALUES ($1::uuid, $2, 'integration_completed', 'wf', $3::uuid, $4::uuid) RETURNING id::text`,
			f.appID, name, f.workingRevID, integ)
	}
	dup := func(name string) string {
		t.Helper()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rev, err := h.duplicateRevision(ctx, tx, f.modelID, name, f.workingRevID, &f.workingRevID)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("duplicateRevision %s: %v", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return rev
	}
	copyRev := dup("Copy")

	got := q(`SELECT COALESCE(source_integration_id::text,'NULL') FROM workflow.automation_rule WHERE revision_id=$1::uuid AND name='OnFeed'`, copyRev)
	want := q(`SELECT id::text FROM model.integration_def WHERE revision_id=$1::uuid AND name='Feed'`, copyRev)
	if got != want {
		t.Errorf("copied OnFeed rule scope = %s, want the copy's Feed connector %s (source Feed %s; NULL fires for ANY connector)", got, want, feed)
	}
	if got, want := q(`SELECT COALESCE(source_integration_id::text,'NULL') FROM workflow.automation_rule WHERE revision_id=$1::uuid AND name='OnTwin'`, copyRev),
		q(`SELECT id::text FROM model.integration_def WHERE revision_id=$1::uuid AND name='Twin'`, copyRev); got != want {
		t.Errorf("copied OnTwin rule scope = %s, want the copy's Twin %s (source %s)", got, want, twin)
	}
	if got := q(`SELECT sc.kind || ':' || sc.interval_seconds || ':' || COALESCE(sc.next_fire_at::text, 'unarmed')
		FROM model.integration_schedule sc JOIN model.integration_def i ON i.id = sc.integration_id
		WHERE i.revision_id=$1::uuid AND i.name='Feed'`, copyRev); got != "interval:3600:unarmed" {
		t.Errorf("copied Feed schedule = %s, want interval:3600:unarmed", got)
	}
	q(`INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, revision_id,
		                                     cron_expr, timezone, misfire_policy, max_retries, retry_backoff_seconds, next_fire_at)
		VALUES ($1::uuid, 'Nightly', 'schedule', 'wf', $2::uuid, '0 2 * * *', 'Europe/Berlin', 'fire_now', 3, 120, now())
		RETURNING id::text`, f.appID, f.workingRevID)

	schedRev := dup("Copy with schedule")
	sched := q(`SELECT concat_ws('|', COALESCE(cron_expr,'NULL'), timezone, misfire_policy, max_retries, retry_backoff_seconds,
		                         COALESCE(next_fire_at::text,'unarmed'))
		FROM workflow.automation_rule WHERE revision_id=$1::uuid AND name='Nightly'`, schedRev)
	if sched != "0 2 * * *|Europe/Berlin|fire_now|3|120|unarmed" {
		t.Errorf("copied schedule rule = %s, want 0 2 * * *|Europe/Berlin|fire_now|3|120|unarmed", sched)
	}
}

// A revision copy gives each dashboard the live revision's grants where the
// live revision has it, the source's elsewhere (decided 2026-10-06): a grant
// revoked on the live revision after a working copy was made used to come
// back when a copy of that working copy went live.
func TestRevisionCopyTakesTheLiveRevisionsGrants(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	live := f.workingRevID // the fixture's active revision
	work := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working copy') RETURNING id::text`, f.modelID)
	ws := q(`SELECT workspace_id::text FROM core.application WHERE id = $1::uuid`, f.appID)
	role := q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Planners') RETURNING id::text`, ws)
	dash := func(rev, name string) string {
		return q(`INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text`, f.modelID, rev, name)
	}
	grant := func(d string) {
		q(`INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid) RETURNING role_id::text`, role, d)
	}
	dash(live, "Budget")          // revoked on the live revision: no grant
	grant(dash(work, "Budget"))   // still granted on the working copy
	grant(dash(work, "New page")) // only the working copy has it

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	copyRev, err := h.duplicateRevision(ctx, tx, f.modelID, "Next", work, &work)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("duplicateRevision: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"Budget": "0", "New page": "1"} {
		got := q(`SELECT count(*)::text FROM identity.business_role_dashboard brd JOIN model.dashboard_def d ON d.id = brd.dashboard_id
			WHERE d.revision_id = $1::uuid AND d.name = $2 AND brd.role_id = $3::uuid`, copyRev, name, role)
		if got != want {
			t.Errorf("%s in the copy has %s grant(s), want %s", name, got, want)
		}
	}
}

// A developer sets which grid a grid mirrors through the cross-dimension
// rollup, and clears it (standing rule 2: only a model import could): a grid
// of the same revision, never itself, never a loop.
func TestDeveloperSetsAGridsRollupSource(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	patch := func(grid, source string) int {
		t.Helper()
		code, _ := f.do(t, "PATCH", "/api/developer/grids/"+grid, "test-approver", map[string]any{"rollup_source_grid_id": source})
		return code
	}
	source := func(grid string) string {
		return q(`SELECT COALESCE(rollup_source_grid_id::text, 'none') FROM model.grid_def WHERE id = $1::uuid`, grid)
	}
	other := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Other') RETURNING id::text`, f.modelID)
	elsewhere := q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Elsewhere', $2::uuid) RETURNING id::text`, f.modelID, other)

	if code := patch(f.gridDeptTotalID, f.gridStaffID); code != http.StatusOK || source(f.gridDeptTotalID) != f.gridStaffID {
		t.Fatalf("set: %d, source %s", code, source(f.gridDeptTotalID))
	}
	for name, c := range map[string]struct{ grid, source string }{
		"itself":           {f.gridStaffID, f.gridStaffID},
		"another revision": {f.gridStaffID, elsewhere},
		"a loop":           {f.gridStaffID, f.gridDeptTotalID},
	} {
		if code := patch(c.grid, c.source); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	if code := patch(f.gridDeptTotalID, ""); code != http.StatusOK || source(f.gridDeptTotalID) != "none" {
		t.Errorf("clear: %d, source %s", code, source(f.gridDeptTotalID))
	}
}
