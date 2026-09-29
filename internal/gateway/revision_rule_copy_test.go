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
// and the whole duplication failed — and comes unarmed (next_fire_at NULL).
// A rule scoped to a connector whose name has twins keeps its reference:
// scoped, never widened, and the duplication does not fail.
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
	newConnector("Twin")
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
	if got := q(`SELECT COALESCE(source_integration_id::text,'NULL') FROM workflow.automation_rule WHERE revision_id=$1::uuid AND name='OnTwin'`, copyRev); got != twin {
		t.Errorf("copied OnTwin rule scope = %s, want the unchanged %s (two copies share the name)", got, twin)
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
