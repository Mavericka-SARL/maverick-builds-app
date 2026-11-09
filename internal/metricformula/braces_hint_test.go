package metricformula

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A formula naming a dimension with a space bare is told to brace it.
func TestParseErrorNamesTheNameToBrace(t *testing.T) {
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
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Brace Co', 'enterprise') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'W') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'A', 'planning') RETURNING id::text`, ws, cust)
	model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, app)
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, model)
	q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid, $2::uuid, 'KPI Name', 'standard') RETURNING id::text`, model, rev)
	q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'kpi_value', true) RETURNING id::text`, model, rev)

	_, err := Validate(ctx, pool, Request{ModelID: model, RevisionID: rev, Name: "followers",
		Formula: `LOOKUP(kpi_value, KPI Name, "FOLLOWERS")`})
	if err == nil || !strings.Contains(err.Error(), "does not parse") || !strings.Contains(err.Error(), "{KPI Name}") {
		t.Fatalf("err = %v, want a parse error naming {KPI Name}", err)
	}
	_, err = Validate(ctx, pool, Request{ModelID: model, RevisionID: rev, Name: "followers",
		Formula: `LOOKUP(kpi_value, {KPI Name}, "FOLLOWERS") +`})
	if err == nil || strings.Contains(err.Error(), "{KPI Name}") {
		t.Errorf("a braced name is hinted at: %v", err)
	}
}
