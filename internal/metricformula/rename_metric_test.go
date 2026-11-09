package metricformula

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A renamed metric's readers follow it: other metrics' formulas (bare and
// braced), a workflow condition, nothing in another revision, and nothing
// that only looks like the name (a string literal, a longer name).
func TestRenameMetricInFormulasRewritesItsReaders(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Rename Co', 'enterprise') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'W') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'A', 'planning') RETURNING id::text`, ws, cust)
	model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, app)
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, model)
	other := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Other') RETURNING id::text`, model)
	metric := func(revID, name, formula string) string {
		return q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid, $2::uuid, $3, $4, NULLIF($5,'')) RETURNING id::text`,
			model, revID, name, formula == "", formula)
	}
	gl := metric(rev, "gl_actuals", "")
	reader := metric(rev, "pnl_actual", `SUMIFS(gl_actuals, Account, "gl_actuals") + {gl_actuals} * 2 + gl_actuals_total`)
	metric(rev, "gl_actuals_total", "")
	elsewhere := metric(other, "pnl_actual", "gl_actuals + 1")
	wf := q(`INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event, steps)
		VALUES ($1::uuid, $2::uuid, 'Close', 'manual', $3::jsonb) RETURNING id::text`, app, rev,
		`[{"id":"gate","type":"condition","due_days":3,"condition":{"formula":"gl_actuals > 0"},"routes":{"true":"a","false":"b"}},{"id":"a","type":"task"}]`)

	if _, err := pool.Exec(ctx, `UPDATE model.metric_def SET name='gl_posted' WHERE id=$1::uuid`, gl); err != nil {
		t.Fatal(err)
	}
	n, err := RenameMetricInFormulas(ctx, pool, gl, "gl_actuals", "gl_posted")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rewrote %d formulas, want 2 (the reader and the condition)", n)
	}
	read := func(sql, id string) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got, want := read(`SELECT formula FROM model.metric_def WHERE id=$1::uuid`, reader),
		`SUMIFS(gl_posted, Account, "gl_actuals") + gl_posted * 2 + gl_actuals_total`; got != want {
		t.Errorf("reader formula = %q, want %q", got, want)
	}
	if got := read(`SELECT formula FROM model.metric_def WHERE id=$1::uuid`, elsewhere); got != "gl_actuals + 1" {
		t.Errorf("another revision's formula changed: %q", got)
	}
	steps := read(`SELECT steps::text FROM workflow.workflow_def WHERE id=$1::uuid`, wf)
	if !strings.Contains(steps, `"formula": "gl_posted > 0"`) || !strings.Contains(steps, `"due_days": 3`) {
		t.Errorf("workflow steps = %s", steps)
	}

	// A change of case only renames nothing.
	if n, err := RenameMetricInFormulas(ctx, pool, gl, "gl_posted", "GL_Posted"); err != nil || n != 0 {
		t.Errorf("case-only rename rewrote %d (err %v)", n, err)
	}
}
