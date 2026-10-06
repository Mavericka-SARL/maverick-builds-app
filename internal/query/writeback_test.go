package query_test

import (
	"context"
	"errors"
	"testing"

	queryv1 "github.com/mavericks-engine/mavericks/gen/go/query/v1"
	"github.com/mavericks-engine/mavericks/internal/query"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// writebackModel is a model with one working revision, one input metric and
// a cost-center dimension holding CC-001 and CC-002 under ALL, on the real
// migrations: the write guard reads core.model and the workflow tables once
// a write names members, which the reduced test schema does not have.
type writebackModel struct {
	store                           *query.Store
	modelID, revisionID, otherRevID string
	metricID, ccDim, otherDim       string
}

func setupWritebackModel(t *testing.T) writebackModel {
	t.Helper()
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
	m := writebackModel{store: query.NewStore(pool)}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('T', 'enterprise') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, ws, cust)
	m.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, app)
	m.revisionID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, m.modelID)
	m.otherRevID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Other') RETURNING id::text`, m.modelID)
	m.metricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'headcount', true) RETURNING id::text`, m.modelID, m.revisionID)
	m.ccDim = q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'cost_center') RETURNING id::text`, m.modelID, m.revisionID)
	all := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'ALL', 'All') RETURNING id::text`, m.ccDim)
	for _, c := range []string{"CC-001", "CC-002"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid, $2, $2, $3::uuid) RETURNING id::text`, m.ccDim, c, all)
	}
	m.otherDim = q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'cost_center') RETURNING id::text`, m.modelID, m.otherRevID)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'CC-001', 'CC-001') RETURNING id::text`, m.otherDim)
	q(`INSERT INTO identity.user (id, keycloak_sub, email, display_name) VALUES ($1::uuid, 'writer', 'writer@example.com', 'Writer') RETURNING id::text`, writebackUser)
	return m
}

const writebackUser = "00000000-0000-0000-0000-000000000099"

func TestWriteback(t *testing.T) {
	m := setupWritebackModel(t)
	updates := []*queryv1.WritebackUpdate{
		{DimMembers: map[string]string{m.ccDim: "CC-001"}, MetricId: m.metricID, Value: 42},
		{DimMembers: map[string]string{m.ccDim: "CC-002"}, MetricId: m.metricID, Value: 18},
	}
	result, err := m.store.Writeback(context.Background(), m.modelID, m.revisionID, writebackUser, updates)
	if err != nil {
		t.Fatalf("Writeback: %v", err)
	}
	if result.CellsWritten != 2 {
		t.Errorf("CellsWritten = %d, want 2", result.CellsWritten)
	}
	if len(result.MetricIDs) != 1 || result.MetricIDs[0] != m.metricID {
		t.Errorf("MetricIDs = %v, want [%s]", result.MetricIDs, m.metricID)
	}
}

// TestWritebackRefusesUnreadCoordinates: a code the dimension does not
// have, a parent member and a dimension of another revision used to be
// stored, where no grid or total reads them; they are refused, as the HTTP
// cell write and the file import refuse them, and nothing is written.
func TestWritebackRefusesUnreadCoordinates(t *testing.T) {
	m := setupWritebackModel(t)
	ctx := context.Background()
	for name, dims := range map[string]map[string]string{
		"unknown code":         {m.ccDim: "CC-404"},
		"parent member":        {m.ccDim: "ALL"},
		"other revision's dim": {m.otherDim: "CC-001"},
		"not a dimension id":   {"cost_center": "CC-001"},
	} {
		_, err := m.store.Writeback(ctx, m.modelID, m.revisionID, writebackUser, []*queryv1.WritebackUpdate{
			{DimMembers: map[string]string{m.ccDim: "CC-001"}, MetricId: m.metricID, Value: 1},
			{DimMembers: dims, MetricId: m.metricID, Value: 2},
		})
		if !errors.Is(err, query.ErrInvalidMember) {
			t.Errorf("%s: err = %v, want ErrInvalidMember", name, err)
		}
	}
	var n int
	if err := m.store.Pool().QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d facts written by refused writebacks, want 0", n)
	}
}
