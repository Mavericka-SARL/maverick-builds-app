package aiassistant_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// The AI's create_revision copies workflows and automation rules as the
// developer console's duplicateRevision (Step H) does: a workflow bound to a
// form ({"form_id"} subject_config) is bound to the copy's form, a rule
// scoped to one connector is scoped to the copy's connector — it used to be
// left NULL, which fires for ANY connector, and connectors were copied only
// after the rules — and a schedule rule keeps its cron settings, where it
// used to fail automation_rule_schedule_cron_chk and abort the whole copy.
func TestCreateRevisionCopiesWorkflowAndRuleRefs(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	srcRev := seedRevision(t, pool, modelID, "Source")
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	appID := q(`SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID)
	gridID := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Plan') RETURNING id::text`, modelID, srcRev)
	formID := q(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid, $2::uuid, 'intake', 'Intake', '[]'::jsonb) RETURNING id::text`, modelID, srcRev)
	feed := q(`INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id, config)
		VALUES ($1::uuid, $2::uuid, 'Feed', 'csv_import', 'grid', $3::uuid, '{}'::jsonb) RETURNING id::text`, modelID, srcRev, gridID)
	q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, subject_type, subject_config, revision_id)
		VALUES ($1::uuid, 'Approve', 'form_submit', 'form', jsonb_build_object('form_id', $2::text), $3::uuid) RETURNING id::text`,
		appID, formID, srcRev)
	q(`INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, revision_id, source_integration_id)
		VALUES ($1::uuid, 'OnFeed', 'integration_completed', 'Approve', $2::uuid, $3::uuid) RETURNING id::text`, appID, srcRev, feed)
	copyOf := func(name string) string {
		t.Helper()
		exec := aiassistant.NewWriteExecutor(pool, modelID, srcRev)
		_, rev, err := exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
			"name": name, "source_revision_id": srcRev,
		}))
		if err != nil {
			t.Fatalf("create_revision %s: %v", name, err)
		}
		return rev
	}
	newRev := copyOf("Copy")

	wantForm := q(`SELECT id::text FROM model.form_def WHERE revision_id=$1::uuid AND name='intake'`, newRev)
	if got := q(`SELECT COALESCE(subject_config->>'form_id','') FROM workflow.workflow_def WHERE revision_id=$1::uuid AND name='Approve'`, newRev); got != wantForm {
		t.Errorf("copied workflow's subject form = %s, want the copy's form %s (source %s)", got, wantForm, formID)
	}
	wantFeed := q(`SELECT id::text FROM model.integration_def WHERE revision_id=$1::uuid AND name='Feed'`, newRev)
	if got := q(`SELECT COALESCE(source_integration_id::text,'NULL') FROM workflow.automation_rule WHERE revision_id=$1::uuid AND name='OnFeed'`, newRev); got != wantFeed {
		t.Errorf("copied OnFeed rule scope = %s, want the copy's Feed connector %s (source %s; NULL fires for ANY connector)", got, wantFeed, feed)
	}
	q(`INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, revision_id,
		                                     cron_expr, timezone, misfire_policy, max_retries, retry_backoff_seconds, next_fire_at)
		VALUES ($1::uuid, 'Nightly', 'schedule', 'Approve', $2::uuid, '0 2 * * *', 'Europe/Berlin', 'fire_now', 3, 120, now())
		RETURNING id::text`, appID, srcRev)

	schedRev := copyOf("Copy with schedule")
	sched := q(`SELECT concat_ws('|', COALESCE(cron_expr,'NULL'), timezone, misfire_policy, max_retries, retry_backoff_seconds,
		                         COALESCE(next_fire_at::text,'unarmed'))
		FROM workflow.automation_rule WHERE revision_id=$1::uuid AND name='Nightly'`, schedRev)
	if sched != "0 2 * * *|Europe/Berlin|fire_now|3|120|unarmed" {
		t.Errorf("copied schedule rule = %s, want 0 2 * * *|Europe/Berlin|fire_now|3|120|unarmed", sched)
	}
}
