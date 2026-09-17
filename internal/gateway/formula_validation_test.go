package gateway

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// Negative tests for the shared formula validation service
// (internal/metricformula), covering each hole the previous per-handler
// checks had. Both create and update run through the same service, so each
// case is asserted on both paths.
//
// The parse case is the one that explains the rest: the old check ran through
// extractFormulaRefs, which returned nil on a parse error, so a formula that
// didn't parse yielded zero references, satisfied the "every reference exists"
// loop vacuously, and was saved — surfacing later as a per-cell #NAME? on the
// grid with nothing pointing back at the formula that caused it.
func TestMetricFormulaValidationRejectsBadFormulas(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	// A second calc metric to build an indirect cycle with:
	//   chain_a = amount, chain_b = chain_a
	// then try to point amount-side chain_a at chain_b.
	var chainAID, chainBID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid, $2::uuid, 'chain_a', false, '=amount') RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&chainAID); err != nil {
		t.Fatalf("seed chain_a: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid, $2::uuid, 'chain_b', false, '=chain_a') RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&chainBID); err != nil {
		t.Fatalf("seed chain_b: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`,
		chainBID, chainAID); err != nil {
		t.Fatalf("seed chain edge: %v", err)
	}

	// A metric in a DIFFERENT revision, to prove references are revision-scoped.
	var otherRevID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Other') RETURNING id::text`, f.modelID).Scan(&otherRevID); err != nil {
		t.Fatalf("seed other revision: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'only_elsewhere', true)`,
		f.modelID, otherRevID); err != nil {
		t.Fatalf("seed foreign-revision metric: %v", err)
	}

	for _, tc := range []struct {
		name    string
		formula string
		why     string
	}{
		{"malformed syntax", "=amount +", "a formula that doesn't parse"},
		{"unbalanced parens", "=SUM(amount", "a formula that doesn't parse"},
		{"unknown function", "=TOTAL(amount)", "a call to a function that doesn't exist"},
		{"self reference", "=new_metric + 1", "a metric referencing itself"},
		{"unknown reference", "=no_such_metric * 2", "a reference to nothing"},
		{"cross-revision reference", "=only_elsewhere + 1", "a reference that exists only in another revision"},
	} {
		t.Run("create/"+tc.name, func(t *testing.T) {
			status, body := doAs(t, f, "POST", "/api/developer/metrics", "rollup-test-approver", f.appID, map[string]any{
				"name": "new_metric", "is_input": false, "formula": tc.formula, "revision_id": f.workingRevID,
			})
			if status != http.StatusBadRequest {
				t.Errorf("creating a metric with %s: status=%d, want 400\nbody: %s", tc.why, status, body)
			}
			var saved int
			if err := f.pool.QueryRow(ctx,
				`SELECT COUNT(*) FROM model.metric_def WHERE model_id=$1::uuid AND name='new_metric'`, f.modelID).Scan(&saved); err != nil {
				t.Fatalf("count metrics: %v", err)
			}
			if saved != 0 {
				t.Errorf("a metric with %s was saved anyway (%d rows)", tc.why, saved)
			}
		})

		t.Run("update/"+tc.name, func(t *testing.T) {
			// chain_a keeps its own name, so "self reference" means
			// referencing chain_a.
			formula := tc.formula
			if tc.name == "self reference" {
				formula = "=chain_a + 1"
			}
			status, body := doAs(t, f, "PATCH", "/api/developer/metrics/"+chainAID, "rollup-test-approver", f.appID, map[string]any{
				"name": "chain_a", "formula": formula, "agg_rule": "sum", "format": "number",
			})
			if status != http.StatusBadRequest {
				t.Errorf("updating a metric to %s: status=%d, want 400\nbody: %s", tc.why, status, body)
			}
			var stored string
			if err := f.pool.QueryRow(ctx, `SELECT COALESCE(formula,'') FROM model.metric_def WHERE id=$1::uuid`, chainAID).Scan(&stored); err != nil {
				t.Fatalf("read formula: %v", err)
			}
			if stored != "=amount" {
				t.Errorf("chain_a's formula is now %q — a rejected update must not persist", stored)
			}
		})
	}

	// Indirect cycle: chain_b already depends on chain_a, so pointing
	// chain_a at chain_b closes the loop.
	status, body := doAs(t, f, "PATCH", "/api/developer/metrics/"+chainAID, "rollup-test-approver", f.appID, map[string]any{
		"name": "chain_a", "formula": "=chain_b + 1", "agg_rule": "sum", "format": "number",
	})
	if status != http.StatusBadRequest {
		t.Errorf("closing an indirect dependency cycle: status=%d, want 400\nbody: %s", status, body)
	}

	// …and a valid formula still saves, with its dependency edge recorded in
	// the same transaction.
	status, body = doAs(t, f, "POST", "/api/developer/metrics", "rollup-test-approver", f.appID, map[string]any{
		"name": "good_metric", "is_input": false, "formula": "=ROUND(amount * 2, 0)", "revision_id": f.workingRevID,
	})
	if status != http.StatusOK {
		t.Fatalf("creating a valid metric: status=%d, want 200\nbody: %s", status, body)
	}
	var edges int
	if err := f.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM model.calc_dependency cd
		JOIN model.metric_def m ON m.id = cd.metric_id
		WHERE m.model_id=$1::uuid AND m.name='good_metric' AND cd.depends_on_metric_id=$2::uuid
	`, f.modelID, f.amountMetricID).Scan(&edges); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edges != 1 {
		t.Errorf("valid metric recorded %d dependency edges on `amount`, want 1", edges)
	}
}

// Aggregation rules were never validated: rollup.combineAgg falls through to
// "sum" for anything it does not recognise, so a typo or an unimplemented rule
// did not fail — it silently computed a wrong total that looked like a real
// number everywhere downstream. That is how "rate" came to be selectable in
// the console while doing nothing.
func TestMetricAggRuleIsValidated(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	cases := []struct {
		name       string
		isInput    bool
		aggRule    string
		wantStatus int
		why        string
	}{
		{"a rule the engine implements", false, "average", http.StatusOK, "average is real"},
		{"formula on a calculated metric", false, "formula", http.StatusOK, "the whole point of the rule"},
		{"formula on an input metric", true, "formula", http.StatusBadRequest, "an input metric has no formula to evaluate"},
		{"a rule nothing implements", false, "median", http.StatusBadRequest, "would have silently summed"},
		{"rate without operands", false, "rate", http.StatusBadRequest, "a ratio with nothing to divide"},
		{"a typo", false, "avg", http.StatusBadRequest, "would have silently summed"},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{
				"name": fmt.Sprintf("agg_case_%d", i), "is_input": c.isInput,
				"agg_rule": c.aggRule, "revision_id": f.workingRevID,
			}
			if !c.isInput {
				body["formula"] = "=amount * 2"
			}
			status, respBody := doAs(t, f, "POST", "/api/developer/metrics", "rollup-test-approver", f.appID, body)
			if status != c.wantStatus {
				t.Fatalf("agg_rule %q on is_input=%v: status=%d, want %d (%s)\nbody: %s",
					c.aggRule, c.isInput, status, c.wantStatus, c.why, respBody)
			}
			// A rejected rule must not reach the column — that is the whole
			// point, since the engine would then silently sum it forever.
			if c.wantStatus == http.StatusBadRequest {
				// By this case's own name, not by agg_rule: an earlier case
				// legitimately creates a metric with agg_rule='formula', and a
				// model-wide count would find that one and report a phantom.
				var n int
				if err := f.pool.QueryRow(ctx,
					`SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid AND name=$2`,
					f.modelID, body["name"]).Scan(&n); err != nil {
					t.Fatalf("read back: %v", err)
				}
				if n != 0 {
					t.Errorf("a rejected agg_rule %q was stored anyway (metric %v exists)", c.aggRule, body["name"])
				}
			}
		})
	}
}

// agg_rule 'rate' is Anaplan's Ratio summary: the total is one metric divided
// by another. The pair is what makes the rule mean anything, so it is checked
// on the way in rather than discovered as a wrong number later — which is
// precisely how "rate" behaved before, sitting in the console quietly summing
// because combineAgg falls through for rules it does not recognise.
func TestRateAggRuleOperandsAreValidated(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	var otherID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'divisor', true) RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&otherID); err != nil {
		t.Fatalf("seed divisor: %v", err)
	}
	// A metric in a DIFFERENT revision: structurally a valid uuid, but its
	// values are not the ones this revision computes against.
	var foreignID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.metric_def (model_id, name, is_input) VALUES ($1::uuid, 'foreign_metric', true) RETURNING id::text`,
		f.modelID).Scan(&foreignID); err != nil {
		t.Fatalf("seed foreign metric: %v", err)
	}

	cases := []struct {
		name        string
		numerator   string
		denominator string
		wantStatus  int
	}{
		{"both operands present", otherID, f.amountMetricID, http.StatusOK},
		{"numerator missing", "", f.amountMetricID, http.StatusBadRequest},
		{"denominator missing", otherID, "", http.StatusBadRequest},
		{"a metric divided by itself", otherID, otherID, http.StatusBadRequest},
		{"an operand from another revision", foreignID, f.amountMetricID, http.StatusBadRequest},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := doAs(t, f, "POST", "/api/developer/metrics", "rollup-test-approver", f.appID, map[string]any{
				"name": fmt.Sprintf("ratio_case_%d", i), "is_input": false, "formula": "=amount * 2",
				"agg_rule": "rate", "revision_id": f.workingRevID,
				"agg_numerator_metric_id": c.numerator, "agg_denominator_metric_id": c.denominator,
			})
			if status != c.wantStatus {
				t.Fatalf("status=%d, want %d\nbody: %s", status, c.wantStatus, body)
			}
		})
	}
}
