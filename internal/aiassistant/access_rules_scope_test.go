package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// TestSetUserAccessRules_KeepsRulesItCannotName is the regression test for
// the AI's set_user_access_rules wiping rules it has no way to name. The
// tool resolves (dimension, code) in the ACTIVE revision only, so a rule on a
// member deleted there (or deleted and re-added under a new lineage) and
// every metric rule are out of its reach. Before the fix it replaced the
// user's ENTIRE rule set: re-affirming "US hidden" dropped the rule on the
// original US — un-hiding revision A's copy — and un-hid the metric in every
// revision.
func TestSetUserAccessRules_KeepsRulesItCannotName(t *testing.T) {
	f := setupAccessRulesFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	revA := f.revID
	usA := q(`SELECT m.id::text FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.revision_id=$1::uuid AND m.code='US'`, revA)
	ukA := q(`SELECT m.id::text FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.revision_id=$1::uuid AND m.code='UK'`, revA)
	metricA := q(`INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id) VALUES ($1::uuid, 'cost', true, 'sum', $2::uuid) RETURNING id::text`, f.modelID, revA)

	// Business admin: US hidden, UK read, metric cost hidden.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeguard.ReplaceUserRules(ctx, tx, f.teamID, []writeguard.RuleInput{
		{Type: "dimension_member", RefID: usA, Access: "hidden"},
		{Type: "dimension_member", RefID: ukA, Access: "read"},
		{Type: "metric", RefID: metricA, Access: "hidden"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Revision B copies A (lineage carried) and becomes active; US is
	// deleted in B and re-added there — a new lineage.
	_, revB, err := f.exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{"name": "B", "source_revision_id": revA}))
	if err != nil {
		t.Fatalf("create_revision: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$2::uuid WHERE id=$1::uuid`, f.modelID, revB); err != nil {
		t.Fatal(err)
	}
	dimB := q(`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='geography'`, revB)
	if _, err := f.pool.Exec(ctx, `DELETE FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='US'`, dimB); err != nil {
		t.Fatal(err)
	}
	usB := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'US', 'US') RETURNING id::text`, dimB)
	ukB := q(`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='UK'`, dimB)
	metricB := q(`SELECT id::text FROM model.metric_def WHERE revision_id=$1::uuid AND name='cost'`, revB)

	// list_users shows what the tool replaces and what it keeps.
	listing, err := aiassistant.NewToolExecutor(f.pool, f.modelID, revB).Execute(ctx, "list_users", nil)
	if err != nil {
		t.Fatalf("list_users: %v", err)
	}
	for _, want := range []string{"geography/UK=read", "cost=hidden", "1 other member rule"} {
		if !strings.Contains(listing, want) {
			t.Errorf("list_users should show %q:\n%s", want, listing)
		}
	}

	// The AI re-affirms US hidden and drops UK's rule.
	if _, _, err := f.exec.Execute(ctx, "set_user_access_rules", mustJSON(t, map[string]any{
		"user_email": "team@example.com",
		"rules":      []map[string]any{{"dimension": "geography", "member_code": "US", "access": "hidden"}},
	})); err != nil {
		t.Fatalf("set_user_access_rules: %v", err)
	}

	dimA, metA, err := writeguard.RuleMaps(ctx, f.pool, f.teamID, revA)
	if err != nil {
		t.Fatal(err)
	}
	if dimA[usA] != "hidden" {
		t.Errorf("old revision A: original US is %q after the AI re-affirmed US hidden, want hidden (rules %v)", dimA[usA], dimA)
	}
	if metA[metricA] != "hidden" {
		t.Errorf("old revision A: metric cost is %q after an AI member-rule call, want hidden", metA[metricA])
	}
	if dimA[ukA] != "" {
		t.Errorf("revision A: UK is %q, want unrestricted — the AI removed that rule and it resolves in the active revision", dimA[ukA])
	}
	dimB2, metB, err := writeguard.RuleMaps(ctx, f.pool, f.teamID, revB)
	if err != nil {
		t.Fatal(err)
	}
	if dimB2[usB] != "hidden" || dimB2[ukB] != "" || metB[metricB] != "hidden" {
		t.Errorf("active revision B: US=%q UK=%q cost=%q, want hidden / unrestricted / hidden", dimB2[usB], dimB2[ukB], metB[metricB])
	}
}
