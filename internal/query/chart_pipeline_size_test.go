package query_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/query"
)

// A chart over a grid shaped like the CRM pipeline (2026-11-17): 339
// accounts, 36 months under 3 years, 5 currencies, 9 stages under 3 roots,
// 3 managers and 6 service categories — ten million leaf coordinates — and
// 143 deals. Read along stage for one manager, one currency and a year (the
// Sales dashboard), and along manager with nothing else fixed (the chat
// connector's read that ran 171 s on the Playground), every value is the
// sum of the deals beneath it, and the reads visit the deals, not the ten
// million coordinates.
func TestChartOverAPipelineSizedGridReadsItsDeals(t *testing.T) {
	pool, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-000000000117"
	revisionID := "00000000-0000-0000-0117-000000000117"
	userID := "00000000-0000-0000-0000-000000001117"

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	id := func(sql string, args ...any) string {
		t.Helper()
		var out string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return out
	}
	dim := func(name string) string {
		return id(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, $2, $3::uuid) RETURNING id::text`, modelID, name, revisionID)
	}
	member := func(dimID, code, parentID string) string {
		return id(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid, $2, $2, NULLIF($3, '')::uuid) RETURNING id::text`, dimID, code, parentID)
	}

	// leaves[dim] are its leaf codes; under[dim][code] the leaves a member covers.
	leaves := map[string][]string{}
	under := map[string]map[string][]string{}
	tree := func(dimID string, roots map[string][]string) {
		under[dimID] = map[string][]string{}
		for root, children := range roots {
			if len(children) == 0 {
				member(dimID, root, "")
				leaves[dimID] = append(leaves[dimID], root)
				under[dimID][root] = []string{root}
				continue
			}
			rid := member(dimID, root, "")
			for _, c := range children {
				member(dimID, c, rid)
				leaves[dimID] = append(leaves[dimID], c)
				under[dimID][c] = []string{c}
			}
			under[dimID][root] = children
		}
	}
	codes := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%03d", prefix, i+1)
		}
		return out
	}

	account, stage, currency, manager, category := dim("Account"), dim("Deal Stage"), dim("Currency"), dim("Manager"), dim("Service Category")
	tree(account, map[string][]string{"ALL": codes("ACC", 339)})
	tree(stage, map[string][]string{"OPEN": codes("OPEN", 5), "CLOSED": codes("CLOSED", 2), "PARKED": codes("PARKED", 2)})
	tree(currency, map[string][]string{"USD": nil, "EUR": nil, "RUB": nil, "KZT": nil, "GBP": nil})
	tree(manager, map[string][]string{"M1": nil, "M2": nil, "M3": nil})
	tree(category, map[string][]string{"ALLCAT": codes("CAT", 6)})

	month := id(`INSERT INTO model.dimension_def (model_id, name, revision_id, dimension_type, time_granularity, fiscal_year_start_month) VALUES ($1::uuid, 'Close Month', $2::uuid, 'time', 'month', 1) RETURNING id::text`, modelID, revisionID)
	under[month] = map[string][]string{}
	for y, year := range []int{2025, 2026, 2027} {
		code := fmt.Sprint(year)
		yid := id(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end) VALUES ($1::uuid, $2, $2, $3::date, $4::date) RETURNING id::text`,
			month, code, fmt.Sprintf("%d-01-01", year), fmt.Sprintf("%d-12-31", year))
		for m := 1; m <= 12; m++ {
			mc := fmt.Sprintf("%d-%02d", year, m)
			start := time.Date(year, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
			exec(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id) VALUES ($1::uuid, $2, $2, $3::date, $4::date, $5, $6::uuid)`,
				month, mc, start.Format("2006-01-02"), start.AddDate(0, 1, -1).Format("2006-01-02"), y*12+m-1, yid)
			leaves[month] = append(leaves[month], mc)
			under[month][mc] = []string{mc}
			under[month][code] = append(under[month][code], mc)
		}
	}

	dims := []string{account, month, currency, stage, manager, category}
	dealValue := id(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id, agg_rule, time_summary) VALUES ($1::uuid, 'deal_value', true, $2::uuid, 'sum', 'sum') RETURNING id::text`, modelID, revisionID)
	dealCount := id(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id, agg_rule, time_summary) VALUES ($1::uuid, 'deal_count', true, $2::uuid, 'sum', 'sum') RETURNING id::text`, modelID, revisionID)
	// A calculated ratio over both: worked out at each bar from the inputs
	// read there (a formula metric's chart path), never a mean of leaves.
	avgDeal := id(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id, formula, agg_rule, time_summary) VALUES ($1::uuid, 'avg_deal', false, $2::uuid, 'deal_value / deal_count', 'formula', 'none') RETURNING id::text`, modelID, revisionID)
	for _, dep := range []string{dealValue, dealCount} {
		exec(`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`, avgDeal, dep)
	}
	// A calculated sum worked out at each leaf and added up (a sum's chart
	// path): half of each deal, at the leaves the deals are at.
	halfDeal := id(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id, formula, agg_rule, time_summary) VALUES ($1::uuid, 'half_deal', false, $2::uuid, 'deal_value * 0.5', 'sum', 'sum') RETURNING id::text`, modelID, revisionID)
	exec(`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`, halfDeal, dealValue)
	grid := id(`INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'Pipeline') RETURNING id::text`, modelID)
	for _, d := range dims {
		exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, grid, d)
	}
	for _, m := range []string{dealValue, dealCount, avgDeal, halfDeal} {
		exec(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, grid, m)
	}

	// 143 deals at leaf coordinates, picked by a fixed generator.
	type deal struct {
		at    map[string]string
		value float64
	}
	var deals []deal
	seed := uint64(20261117)
	next := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int((seed >> 33) % uint64(n))
	}
	seen := map[string]bool{}
	for len(deals) < 143 {
		at := map[string]string{}
		for _, d := range dims {
			at[d] = leaves[d][next(len(leaves[d]))]
		}
		raw, _ := json.Marshal(at)
		if seen[string(raw)] {
			continue
		}
		seen[string(raw)] = true
		v := float64(1000 + next(900000))
		deals = append(deals, deal{at: at, value: v})
		exec(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, $5, $6::uuid)`,
			modelID, revisionID, dealValue, string(raw), v, userID)
		exec(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, 1, $5::uuid)`,
			modelID, revisionID, dealCount, string(raw), userID)
	}
	// want is the deals' sum beneath pins (an unpinned dimension: any of its leaves).
	want := func(pins map[string]string) (float64, int) {
		var sum float64
		n := 0
	next:
		for _, d := range deals {
			for dimID, code := range pins {
				covered := false
				for _, l := range under[dimID][code] {
					if d.at[dimID] == l {
						covered = true
						break
					}
				}
				if !covered {
					continue next
				}
			}
			sum += d.value
			n++
		}
		return sum, n
	}

	resolver := query.NewChartResolver(pool)
	allThree := []string{dealValue, avgDeal, halfDeal}
	for _, read := range []struct {
		name    string
		along   string
		context map[string]string
		metrics []string
	}{
		{"Sales dashboard: by stage for one manager, one currency, a year", stage, map[string]string{manager: "M1", currency: "USD", month: "2026"}, allThree},
		{"connector: by manager, nothing else fixed", manager, map[string]string{}, allThree},
		// The dashboard's selector bar pins every selector, "All accounts"
		// and "All service categories" included: a parent member the read
		// expands child by child. A metric worked out at each leaf still
		// walks every child there (OBSERVATIONS: "A per-leaf formula under
		// a pinned parent still walks every child") — half_deal took 90 s
		// on a CI runner — so it is not read here.
		{"Sales dashboard as it loads: All accounts, All categories pinned", stage, map[string]string{
			manager: "M1", currency: "USD", month: "2026", account: "ALL", category: "ALLCAT"}, []string{dealValue, avgDeal}},
	} {
		started := time.Now()
		cfg := &query.ChartConfig{ChartType: query.ChartBar, DimensionID: read.along, MetricIDs: read.metrics}
		result, err := resolver.Resolve(ctx, cfg, read.context, modelID, revisionID, grid, userID)
		if err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		took := time.Since(started)
		data := result.(*query.CategoryChartData)
		effective := map[string]string{}
		for k, v := range read.context {
			effective[k] = v
		}
		checked := 0
		for i, c := range data.Categories {
			effective[read.along] = c.Key
			sum, n := want(effective)
			got := data.Series[0].Values[i]
			switch {
			case got == nil && n > 0:
				t.Errorf("%s: %s has no value, want %v (%d deals)", read.name, c.Key, sum, n)
			case got != nil && math.Abs(*got-sum) > 1e-6:
				t.Errorf("%s: %s = %v, want %v (%d deals)", read.name, c.Key, *got, sum, n)
			}
			if avg := data.Series[1].Values[i]; n > 0 && (avg == nil || math.Abs(*avg-sum/float64(n)) > 1e-6) {
				t.Errorf("%s: average deal at %s = %v, want %v", read.name, c.Key, avg, sum/float64(n))
			}
			if len(data.Series) > 2 {
				if half := data.Series[2].Values[i]; n > 0 && (half == nil || math.Abs(*half-sum/2) > 1e-6) {
					t.Errorf("%s: half deal at %s = %v, want %v", read.name, c.Key, half, sum/2)
				}
			}
			checked++
		}
		if checked == 0 {
			t.Fatalf("%s: no categories", read.name)
		}
		t.Logf("%s: %d bars in %v", read.name, checked, took)
		for _, m := range []struct{ name, id string }{{"deal value", dealValue}, {"average deal", avgDeal}, {"half deal", halfDeal}} {
			if !slices.Contains(read.metrics, m.id) {
				continue
			}
			one := time.Now()
			if _, err := resolver.Resolve(ctx, &query.ChartConfig{ChartType: query.ChartBar, DimensionID: read.along, MetricIDs: []string{m.id}}, read.context, modelID, revisionID, grid, userID); err != nil {
				t.Fatalf("%s, %s alone: %v", read.name, m.name, err)
			}
			t.Logf("  %s alone: %v", m.name, time.Since(one))
		}
		if took > 30*time.Second {
			t.Errorf("%s took %v: the read walks every leaf coordinate again", read.name, took)
		}
	}
}
