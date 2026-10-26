package gateway

import (
	"context"
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
