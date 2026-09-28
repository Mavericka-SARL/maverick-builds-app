// Builds a whole planning model using nothing but the AI Developer: the
// assistant proposes, a developer confirms, and every dimension, member,
// metric and grid arrives through a write tool. No model-authoring HTTP
// endpoint is called.
//
// It answers two questions that the tool-level tests cannot. Whether the tool
// set is actually sufficient to build something real — 22 steps of it,
// chained by ID — and whether the ownership guards added to those tools let
// legitimate work through, which a guard that refused everything would also
// pass its own test while breaking the product.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/salesdemo"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// proposeStep is one proposed action, in the shape prompt.go teaches the model to
// emit. "<created in step N>" is substituted with that step's created ID at
// execution time, which is how a build chains without knowing IDs in advance.
func proposeStep(tool, description string, params map[string]any) map[string]any {
	return map[string]any{"tool": tool, "description": description, "params": params}
}

func ref(n int) string { return fmt.Sprintf("<created in step %d>", n) }

func TestAIDeveloperBuildsAWholeModel(t *testing.T) {
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('AIBuildCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Test app', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Test model') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID)

	devSub := "aibuild-dev"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,'dev@aibuild.co','Dev',$2::uuid) RETURNING id::text`, devSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid)`, devID, wsID)

	// ── What the assistant proposes ────────────────────────────────────────
	// The sales-planning model from internal/salesdemo, step for step: three
	// three-level dimensions, the eight metrics covering every aggregation
	// rule the engine implements, and the Sales Plan grid. Three levels is
	// deliberate there and matters here too — a two-level hierarchy would not
	// show whether a member created in step N can parent one created in
	// step N+1 whose own parent was also created in this proposal.
	var steps []map[string]any
	add := func(tool, description string, params map[string]any) int {
		steps = append(steps, proposeStep(tool, description, params))
		return len(steps) // 1-based step number, for ref()
	}

	type node struct{ code, label, parent string }
	dimSteps := map[string]int{}
	memberSteps := map[string]int{}
	buildDim := func(name string, nodes []node) {
		dimSteps[name] = add("create_dimension", "Create dimension '"+name+"'",
			map[string]any{"name": name})
		for _, n := range nodes {
			params := map[string]any{
				"dimension_id": ref(dimSteps[name]), "code": n.code, "label": n.label,
			}
			if n.parent != "" {
				params["parent_code"] = n.parent
			}
			memberSteps[name+"/"+n.code] = add("add_dimension_member", "Add "+n.label, params)
		}
	}

	buildDim("geography", []node{
		{"WORLD", "World", ""},
		{"EMEA", "EMEA", "WORLD"}, {"AMER", "Americas", "WORLD"},
		{"UK", "United Kingdom", "EMEA"}, {"DE", "Germany", "EMEA"},
		{"US", "United States", "AMER"}, {"CA", "Canada", "AMER"},
	})
	buildDim("product", []node{
		{"ALL_PROD", "All products", ""},
		{"HARDWARE", "Hardware", "ALL_PROD"}, {"SOFTWARE", "Software", "ALL_PROD"},
		{"LAPTOP", "Laptop", "HARDWARE"}, {"MONITOR", "Monitor", "HARDWARE"},
		{"LICENSE", "Licence", "SOFTWARE"}, {"SUPPORT", "Support", "SOFTWARE"},
	})
	// period is a TIME dimension, built the way the AI must: the type is
	// declared on create_dimension, leaf quarters carry their dates, and
	// H1/H2/FY26 are undated aggregates above them.
	dimSteps["period"] = add("create_dimension", "Create time dimension 'period'", map[string]any{
		"name": "period", "dimension_type": "time", "time_granularity": "quarter", "fiscal_year_start_month": 1,
	})
	for _, q := range salesdemo.Periods {
		params := map[string]any{"dimension_id": ref(dimSteps["period"]), "code": q.Code, "label": q.Label}
		if q.Start != "" {
			params["period_start"], params["period_end"] = q.Start, q.End
		}
		if q.Parent != "" {
			params["parent_code"] = q.Parent
		}
		memberSteps["period/"+q.Code] = add("add_dimension_member", "Add "+q.Label, params)
	}

	metricSteps := map[string]int{}
	for _, in := range []string{"units", "revenue", "cost", "target"} {
		metricSteps[in] = add("create_metric", "Input metric '"+in+"'",
			map[string]any{"name": in, "is_input": true, "agg_rule": "sum", "format": "number"})
	}
	metricSteps["margin"] = add("create_metric", "Calculated 'margin'", map[string]any{
		"name": "margin", "is_input": false, "formula": "={revenue} - {cost}",
		"agg_rule": "sum", "format": "number",
	})
	// Percentages total as the formula run against aggregated inputs.
	metricSteps["margin_pct"] = add("create_metric", "Calculated 'margin_pct'", map[string]any{
		"name": "margin_pct", "is_input": false, "formula": "={margin} / {revenue} * 100",
		"agg_rule": "formula", "format": "number",
	})
	metricSteps["attainment_pct"] = add("create_metric", "Calculated 'attainment_pct'", map[string]any{
		"name": "attainment_pct", "is_input": false, "formula": "={revenue} / {target} * 100",
		"agg_rule": "formula", "format": "number",
	})
	// A ratio of two other metrics — the operands are steps in this same
	// proposal, so they are references, not UUIDs the assistant could know.
	metricSteps["avg_price"] = add("create_metric", "Calculated 'avg_price' as a ratio", map[string]any{
		"name": "avg_price", "is_input": false, "formula": "={revenue} / {units}",
		"agg_rule": "rate", "format": "number",
		"agg_numerator_metric_id":   ref(metricSteps["revenue"]),
		"agg_denominator_metric_id": ref(metricSteps["units"]),
	})

	// The forecast: two more inputs and the 24 calculated metrics that
	// exercise 24 formula functions between them (LAG and PREVIOUS along the
	// time dimension included). This is the part the old hand-rolled
	// validator could not have built at all — every one of these formulas
	// carries either a function call or a {reference}, and it read both as
	// missing metric names.
	for _, in := range []string{"seasonality", "pipeline"} {
		metricSteps[in] = add("create_metric", "Input metric '"+in+"'",
			map[string]any{"name": in, "is_input": true, "agg_rule": "sum", "format": "number"})
	}
	for _, f := range salesdemo.ForecastMetrics {
		metricSteps[f.Name] = add("create_metric", "Calculated '"+f.Name+"'", map[string]any{
			"name": f.Name, "is_input": false, "formula": f.Formula,
			"agg_rule": f.Agg, "format": "number",
		})
	}

	gridStep := add("create_grid", "Create the 'Sales Plan' grid", map[string]any{"name": "Sales Plan"})
	for _, d := range []string{"geography", "product", "period"} {
		add("add_grid_dimension", "Put "+d+" on the grid",
			map[string]any{"grid_id": ref(gridStep), "dimension_id": ref(dimSteps[d])})
	}
	for _, name := range salesdemo.MetricNames {
		add("add_grid_metric", "Add "+name+" to the grid",
			map[string]any{"grid_id": ref(gridStep), "metric_id": ref(metricSteps[name])})
	}

	// The proposal cap is 50 steps (server-enforced batching), so the build
	// ships as TWO proposals — which also makes this a parity test for the
	// batch contract itself: a later batch cannot use "<created in step N>"
	// placeholders for entities created in an earlier, already-executed
	// batch; it references them by NAME, exactly as the prompt instructs the
	// model to and requireInModel resolves. Within a batch, placeholders are
	// renumbered to the batch's own 1-based positions.
	stepName := make([]string, len(steps)) // 0-based: creating step -> entity name
	for i, st := range steps {
		tool, _ := st["tool"].(string)
		if tool == "create_dimension" || tool == "create_metric" || tool == "create_grid" {
			if params, ok := st["params"].(map[string]any); ok {
				stepName[i], _ = params["name"].(string)
			}
		}
	}
	const batchSize = 50
	rebatch := func(batch []map[string]any, offset int) []map[string]any {
		out := make([]map[string]any, 0, len(batch))
		for _, st := range batch {
			params, _ := st["params"].(map[string]any)
			newParams := map[string]any{}
			for k, v := range params {
				sv, isStr := v.(string)
				if !isStr || !strings.HasPrefix(sv, "<created in step ") {
					newParams[k] = v
					continue
				}
				var n int
				_, _ = fmt.Sscanf(sv, "<created in step %d>", &n)
				if n > offset { // same batch: renumber to batch-local position
					newParams[k] = ref(n - offset)
				} else { // earlier batch: reference by name
					if stepName[n-1] == "" {
						t.Fatalf("step %d referenced across batches but created no named entity", n)
					}
					newParams[k] = stepName[n-1]
				}
			}
			out = append(out, map[string]any{"tool": st["tool"], "description": st["description"], "params": newParams})
		}
		return out
	}
	var resps []providers.ChatResponse
	for off := 0; off < len(steps); off += batchSize {
		end := off + batchSize
		if end > len(steps) {
			end = len(steps)
		}
		args, _ := json.Marshal(map[string]any{"steps": rebatch(steps[off:end], off)})
		resps = append(resps, providers.ChatResponse{
			FinishReason: "tool_calls",
			Message: providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{
				{ID: fmt.Sprintf("call_%d", off/batchSize+1), Name: "propose_actions", Arguments: args},
			}},
		})
	}
	fake := &multiScriptProvider{resps: resps}

	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, testProvider: fake}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)

	do := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Dev-User", devSub)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.Bytes()
	}

	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, appID, devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// Pre-title: the auto-namer would otherwise consume the first scripted
	// response (it fires an extra Chat() on an untitled session's first
	// message), desynchronizing the per-call script.
	if err := chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled"); err != nil {
		t.Fatalf("pre-title: %v", err)
	}

	pStore := aiassistant.NewProposalStore(pool)

	// Nothing exists before any confirmation: proposing is not writing.
	assertNoWrites := func() {
		t.Helper()
		var before int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid`, modelID).Scan(&before)
		if before != 0 {
			t.Fatalf("%d metric(s) existed before the proposal was confirmed", before)
		}
	}

	// One ask per batch: message -> pending proposal -> confirm -> continue.
	for round := 1; round <= len(resps); round++ {
		content := "build me a sales plan"
		if round > 1 {
			content = "continue with the remaining steps"
		}
		if status, body := do("POST", "/api/ai/sessions/"+sess.ID+"/messages",
			map[string]string{"content": content}); status != http.StatusOK {
			t.Fatalf("send message (round %d): status %d\n%s", round, status, body)
		}
		proposals, err := pStore.ListProposals(ctx, sess.ID)
		if err != nil || len(proposals) != round {
			t.Fatalf("round %d: expected %d proposal(s), got %d (err %v)", round, round, len(proposals), err)
		}
		var pending *aiassistant.Proposal
		for i := range proposals {
			if proposals[i].Status == "pending" {
				pending = &proposals[i]
			}
		}
		if pending == nil {
			t.Fatalf("round %d: no pending proposal — an AI write must wait for confirmation", round)
		}
		if round == 1 {
			assertNoWrites()
		}
		status, body := do("POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+pending.ID+"/confirm", nil)
		if status != http.StatusOK {
			t.Fatalf("confirm (round %d): status %d\n%s", round, status, body)
		}
		confirmed, err := pStore.GetProposal(ctx, pending.ID)
		if err != nil {
			t.Fatalf("re-read proposal: %v", err)
		}
		if confirmed.Status != "executed" {
			for i, s := range confirmed.Steps {
				if s.Status != "success" {
					t.Errorf("round %d step %d (%s) %s: %s", round, i+1, s.Tool, s.Status, s.Result)
				}
			}
			t.Fatalf("round %d proposal finished %q, want executed", round, confirmed.Status)
		}
	}

	// 3. The model exists. AI writes land in an isolated draft revision, never
	//    the active one, so that is where to look.
	var draftRev string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(draft_revision_id::text,'') FROM ai_assistant.session WHERE id=$1::uuid`, sess.ID).Scan(&draftRev); err != nil {
		t.Fatalf("read draft revision: %v", err)
	}
	if draftRev == "" || draftRev == revID {
		t.Fatalf("draft revision is %q — AI writes must not target the active revision", draftRev)
	}

	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	if n := count(`SELECT count(*) FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, draftRev); n != 3 {
		t.Errorf("%d dimensions in the draft, want 3", n)
	}
	if n := count(`
		SELECT count(*) FROM model.dimension_member m
		JOIN model.dimension_def d ON d.id = m.dimension_id
		WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid`, modelID, draftRev); n != 21 {
		t.Errorf("%d dimension members in the draft, want 21 (3 dimensions x 7)", n)
	}
	// The time dimension arrived as one, with its periods in order.
	var periodType string
	var periodIndexes []int
	if err := pool.QueryRow(ctx, `SELECT dimension_type FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='period'`,
		modelID, draftRev).Scan(&periodType); err != nil || periodType != "time" {
		t.Errorf("period dimension_type = %q (%v), want time", periodType, err)
	}
	if prow, err := pool.Query(ctx, `SELECT m.time_index FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id
		WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name='period' AND m.period_start IS NOT NULL ORDER BY m.period_start`, modelID, draftRev); err == nil {
		for prow.Next() {
			var i int
			_ = prow.Scan(&i)
			periodIndexes = append(periodIndexes, i)
		}
		prow.Close()
	}
	if fmt.Sprint(periodIndexes) != "[0 1 2 3]" {
		t.Errorf("period time_index = %v, want [0 1 2 3]", periodIndexes)
	}
	wantMetrics := len(salesdemo.MetricNames) + 2 + len(salesdemo.ForecastMetrics)
	if n := count(`SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`,
		modelID, draftRev); n != wantMetrics {
		t.Errorf("%d metrics in the draft, want %d", n, wantMetrics)
	}

	// Every hierarchy is three levels deep and chained through steps that did
	// not exist when the proposal was written. UK's parent was created two
	// steps earlier and its grandparent three, all by reference.
	for _, c := range []struct{ dim, child, parent, grandparent string }{
		{"geography", "UK", "EMEA", "WORLD"},
		{"product", "LAPTOP", "HARDWARE", "ALL_PROD"},
		{"period", "Q3", "H2", "FY26"},
	} {
		var parent, grandparent string
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(p.code,''), COALESCE(g.code,'')
			FROM model.dimension_member m
			JOIN model.dimension_def d ON d.id = m.dimension_id
			LEFT JOIN model.dimension_member p ON p.id = m.parent_member_id
			LEFT JOIN model.dimension_member g ON g.id = p.parent_member_id
			WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name=$3 AND m.code=$4
		`, modelID, draftRev, c.dim, c.child).Scan(&parent, &grandparent); err != nil {
			t.Fatalf("read %s/%s ancestry: %v", c.dim, c.child, err)
		}
		if parent != c.parent || grandparent != c.grandparent {
			t.Errorf("%s/%s ancestry is %s <- %s, want %s <- %s",
				c.dim, c.child, parent, grandparent, c.parent, c.grandparent)
		}
	}

	// The grid holds all three dimensions and every metric.
	var gridID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Sales Plan'`,
		modelID, draftRev).Scan(&gridID); err != nil {
		t.Fatalf("find the grid: %v", err)
	}
	if n := count(`SELECT count(*) FROM model.grid_dimension WHERE grid_id=$1::uuid`, gridID); n != 3 {
		t.Errorf("%d grid dimensions, want 3", n)
	}
	if n := count(`SELECT count(*) FROM model.grid_metric WHERE grid_id=$1::uuid`, gridID); n != len(salesdemo.MetricNames) {
		t.Errorf("%d grid metrics, want %d", n, len(salesdemo.MetricNames))
	}

	// The formulas themselves round-tripped intact. A validator that stripped
	// or rewrote what it did not understand would still produce the right
	// count of metrics carrying the wrong expressions.
	for _, f := range salesdemo.ForecastMetrics {
		var stored string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(formula,'') FROM model.metric_def
			 WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
			modelID, draftRev, f.Name).Scan(&stored); err != nil {
			t.Errorf("read %s: %v", f.Name, err)
			continue
		}
		if stored != f.Formula {
			t.Errorf("%s formula stored as %q, want %q", f.Name, stored, f.Formula)
		}
	}

	// The aggregation rules survived. A build that produced the right metrics
	// but defaulted every rule to "sum" would pass every count above and be
	// wrong in every total on the screen.
	wantRule := map[string]string{
		"units": "sum", "revenue": "sum", "cost": "sum", "target": "sum",
		"margin": "sum", "margin_pct": "formula", "attainment_pct": "formula", "avg_price": "rate",
		"seasonality": "sum", "pipeline": "sum",
	}
	for _, f := range salesdemo.ForecastMetrics {
		wantRule[f.Name] = f.Agg
	}
	rows, err := pool.Query(ctx,
		`SELECT name, agg_rule FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`,
		modelID, draftRev)
	if err != nil {
		t.Fatalf("read agg rules: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	seen := map[string]bool{}
	for rows.Next() {
		var name, rule string
		if err := rows.Scan(&name, &rule); err != nil {
			t.Fatalf("scan agg rule: %v", err)
		}
		seen[name] = true
		if want, ok := wantRule[name]; !ok {
			t.Errorf("unexpected metric %q in the draft", name)
		} else if rule != want {
			t.Errorf("%s agg_rule is %q, want %q", name, rule, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate agg rules: %v", err)
	}
	for name := range wantRule {
		if !seen[name] {
			t.Errorf("metric %q is missing from the draft", name)
		}
	}

	// avg_price is the one metric that needs more than a rule name: "rate"
	// divides two nominated metrics, and both were created inside this same
	// proposal, so they arrived as step references rather than UUIDs. If
	// either is NULL the metric saves clean and then fails in the scheduler
	// on every recalculation, which is a long way from where it was authored.
	var numName, denName string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(n.name,''), COALESCE(d.name,'')
		FROM model.metric_def m
		LEFT JOIN model.metric_def n ON n.id = m.agg_numerator_metric_id
		LEFT JOIN model.metric_def d ON d.id = m.agg_denominator_metric_id
		WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid AND m.name='avg_price'
	`, modelID, draftRev).Scan(&numName, &denName); err != nil {
		t.Fatalf("read avg_price operands: %v", err)
	}
	if numName != "revenue" || denName != "units" {
		t.Errorf("avg_price is %q / %q, want revenue / units", numName, denName)
	}

	// Dependency edges, which decide recalculation order. margin depends on
	// revenue and cost; nothing reports a metric that saved with none.
	if n := count(`
		SELECT count(*) FROM model.calc_dependency cd
		JOIN model.metric_def m ON m.id = cd.metric_id
		WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid AND m.name='margin'
	`, modelID, draftRev); n != 2 {
		t.Errorf("margin has %d dependency edge(s), want 2", n)
	}
}

// dimensionalParityMetrics are the formulas both paths below save, in order:
// a typed property, a criteria sum over a property, a literal LOOKUP and a
// count over the dimension (the numbers of dimensional_formulas_test.go),
// then a LOOKUP whose member is an expression, the Excel-order SUMIF,
// AVERAGEIF and COUNTIF, and the calendar functions.
var dimensionalParityMetrics = []struct{ name, formula string }{
	{"scaled", `revenue * region.factor`},
	{"smb", `SUMIFS(revenue, region.segment, "SMB")`},
	{"emea", `LOOKUP(revenue, region, "EMEA")`},
	{"n_regions", `COUNTIFS(region, "*")`},
	{"peer_rev", `LOOKUP(revenue, region, region.peer)`},
	{"smb_if", `SUMIF(region.segment, "SMB", revenue)`},
	{"avg_if", `AVERAGEIF(region, "*", revenue)`},
	{"n_big", `COUNTIF(region.factor, ">=3")`},
	{"days", `DAYSINMONTH(2028, 2) + DAYSINYEAR(2027)`},
}

// dimensionalParityWant is what those metrics compute with EMEA (segment
// SMB, factor 2, peer US, revenue 100) and US (segment ENT, factor 3, peer
// EMEA, revenue 50).
var dimensionalParityWant = map[string]float64{
	"scaled:EMEA": 200, "scaled:US": 150,
	"smb:EMEA": 100, "smb:US": 100,
	"emea:EMEA": 100, "emea:US": 100,
	"n_regions:EMEA": 2, "n_regions:US": 2,
	"peer_rev:EMEA": 50, "peer_rev:US": 100,
	"smb_if:EMEA": 100, "smb_if:US": 100,
	"avg_if:EMEA": 75, "avg_if:US": 75,
	"n_big:EMEA": 1, "n_big:US": 1,
	"days:EMEA": 29 + 365, "days:US": 29 + 365,
}

// cellKey is a grid cell keyed by metric NAME and its member codes in sorted
// order ("lagged:EMEA:Q2"), so the key does not depend on the order a path
// happened to give the metric's dimensions.
func cellKey(metric string, codes ...string) string {
	sorted := append([]string(nil), codes...)
	sort.Strings(sorted)
	return strings.Join(append([]string{metric}, sorted...), ":")
}

// gridValuesByName reads GET /api/grid and re-keys its cells and totals by
// metric NAME ("scaled:EMEA", "scaled:TOTAL"; see cellKey), so two models
// built by different paths — with different IDs — compare directly. It polls until
// every cell in want has its value (recalculation runs in the background)
// and fails the test when one never arrives.
func gridValuesByName(t *testing.T, get func(path string) (int, []byte), gridID string, want map[string]float64) map[string]float64 {
	t.Helper()
	var got map[string]float64
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		status, raw := get("/api/grid?grid_id=" + gridID)
		if status != http.StatusOK {
			t.Fatalf("GET /api/grid: status %d\n%s", status, raw)
		}
		var resp struct {
			Metrics []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"metrics"`
			Cells    map[string]float64 `json:"cells"`
			Totals   map[string]float64 `json:"totals"`
			Withheld []string           `json:"withheld"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode grid: %v", err)
		}
		names := map[string]string{}
		for _, m := range resp.Metrics {
			names[m.ID] = m.Name
		}
		got = map[string]float64{}
		for key, v := range resp.Cells {
			parts := strings.Split(key, ":")
			got[cellKey(names[parts[0]], parts[1:]...)] = v
		}
		for id, v := range resp.Totals {
			got[names[id]+":TOTAL"] = v
		}
		if len(resp.Withheld) > 0 {
			t.Fatalf("a developer with no access rules was withheld %v", resp.Withheld)
		}
		settled := true
		for k, w := range want {
			if v, ok := got[k]; !ok || !nearly(v, w) {
				settled = false
			}
		}
		if settled {
			return got
		}
		if time.Now().After(deadline) {
			for k, w := range want {
				if v, ok := got[k]; !ok || !nearly(v, w) {
					t.Errorf("grid %s = %v (present %v), want %v", k, v, ok, w)
				}
			}
			t.FailNow()
		}
	}
}

// developerDimensionalGrid builds the parity model the developer's way —
// the console's HTTP endpoints — and returns its grid by metric name.
func developerDimensionalGrid(t *testing.T) map[string]float64 {
	t.Helper()
	f := setupDimFormulaFixture(t) // region {EMEA, US}, input revenue, grid "Plan"
	props := "/api/developer/dimensions/" + f.region + "/properties"
	f.call("POST", props, map[string]any{"name": "segment", "data_type": "text"})
	f.call("POST", props, map[string]any{"name": "factor", "data_type": "number"})
	f.call("POST", props, map[string]any{"name": "peer", "data_type": "text"})
	f.setMember("EMEA", map[string]string{"segment": "SMB", "factor": "2", "peer": "US"})
	f.setMember("US", map[string]string{"segment": "ENT", "factor": "3", "peer": "EMEA"})
	for _, m := range dimensionalParityMetrics {
		f.metric[m.name] = f.call("POST", "/api/developer/metrics", f.metricBody(m.name, m.formula))
		f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric[m.name], nil)
	}
	f.writeCell("revenue", "US", 50)
	f.writeCell("revenue", "EMEA", 100)
	return gridValuesByName(t, func(path string) (int, []byte) { return f.req("GET", path, nil) }, f.gridID, dimensionalParityWant)
}

// aiBuild is one AI Developer build: a fresh tenant with a developer, a chat
// session whose scripted model answers with one propose_actions call, and
// that proposal confirmed through the real endpoint. No model-authoring
// endpoint is called.
type aiBuild struct {
	t                     *testing.T
	pool                  *pgxpool.Pool
	modelID, revID, draft string // revID is the active revision the build started from
	sessID                string
	do                    func(method, path string, body any) (int, []byte)
	fake                  *multiScriptProvider
}

// q scans the single text column of a one-row query.
func (b *aiBuild) q(sql string, args ...any) string {
	b.t.Helper()
	var id string
	if err := b.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		b.t.Fatalf("query %q: %v", sql, err)
	}
	return id
}

// promote makes the AI's draft the active revision through the session's
// promote endpoint and returns it.
func (b *aiBuild) promote() string {
	b.t.Helper()
	if status, body := b.do("POST", "/api/ai/sessions/"+b.sessID+"/promote-draft", nil); status != http.StatusOK {
		b.t.Fatalf("promote: status %d\n%s", status, body)
	}
	active := activeRevisionID(b.t, b.pool, b.modelID)
	if active != b.draft {
		b.t.Fatalf("active revision %s after promote, want the draft %s", active, b.draft)
	}
	return active
}

// runAIBuild has the assistant propose steps in answer to request, confirms
// the proposal and fails the test unless every step succeeded and the
// writes landed in a draft of their own.
func runAIBuild(t *testing.T, request string, steps []map[string]any) *aiBuild {
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
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('AIDimCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Dim app', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Dim model') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID); err != nil {
		t.Fatal(err)
	}
	devSub := "aidim-dev"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,'dev@aidim.co','Dev',$2::uuid) RETURNING id::text`, devSub, custID)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid)`, devID, wsID); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]any{"steps": steps})
	fake := &multiScriptProvider{resps: []providers.ChatResponse{{
		FinishReason: "tool_calls",
		Message: providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{
			{ID: "call_1", Name: "propose_actions", Arguments: args},
		}},
	}}}
	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, testProvider: fake}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)
	do := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Dev-User", devSub)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.Bytes()
	}

	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, appID, devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled"); err != nil {
		t.Fatalf("pre-title: %v", err)
	}
	if status, body := do("POST", "/api/ai/sessions/"+sess.ID+"/messages",
		map[string]string{"content": request}); status != http.StatusOK {
		t.Fatalf("send message: status %d\n%s", status, body)
	}
	pStore := aiassistant.NewProposalStore(pool)
	proposals, err := pStore.ListProposals(ctx, sess.ID)
	if err != nil || len(proposals) != 1 || proposals[0].Status != "pending" {
		t.Fatalf("want one pending proposal, got %+v (err %v)", proposals, err)
	}
	if status, body := do("POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposals[0].ID+"/confirm", nil); status != http.StatusOK {
		t.Fatalf("confirm: status %d\n%s", status, body)
	}
	confirmed, err := pStore.GetProposal(ctx, proposals[0].ID)
	if err != nil {
		t.Fatalf("re-read proposal: %v", err)
	}
	if confirmed.Status != "executed" {
		for i, s := range confirmed.Steps {
			if s.Status != "success" {
				t.Errorf("step %d (%s) %s: %s", i+1, s.Tool, s.Status, s.Result)
			}
		}
		t.Fatalf("proposal finished %q, want executed", confirmed.Status)
	}

	draft, err := chatStore.GetSession(ctx, sess.ID)
	if err != nil || draft.DraftRevisionID == "" || draft.DraftRevisionID == revID {
		t.Fatalf("draft revision %q (err %v) — AI writes must land in their own draft", draft.DraftRevisionID, err)
	}
	return &aiBuild{t: t, pool: pool, modelID: modelID, revID: revID, draft: draft.DraftRevisionID, sessID: sess.ID, do: do, fake: fake}
}

// proposeAgain has the assistant answer request in the same session with
// one more propose_actions call, confirms that proposal through the real
// endpoint and returns it as executed. Unlike runAIBuild it does not
// require every step to succeed: a caller checking a refusal reads the
// failed step's result.
func (b *aiBuild) proposeAgain(request string, steps []map[string]any) aiassistant.Proposal {
	b.t.Helper()
	ctx := context.Background()
	args, _ := json.Marshal(map[string]any{"steps": steps})
	b.fake.resps = append(b.fake.resps, providers.ChatResponse{
		FinishReason: "tool_calls",
		Message: providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{
			{ID: fmt.Sprintf("call_%d", len(b.fake.resps)+1), Name: "propose_actions", Arguments: args},
		}},
	})
	if status, body := b.do("POST", "/api/ai/sessions/"+b.sessID+"/messages",
		map[string]string{"content": request}); status != http.StatusOK {
		b.t.Fatalf("send message: status %d\n%s", status, body)
	}
	pStore := aiassistant.NewProposalStore(b.pool)
	pending, err := pStore.ListPendingProposals(ctx, b.sessID)
	if err != nil || len(pending) != 1 {
		b.t.Fatalf("want one pending proposal, got %+v (err %v)", pending, err)
	}
	if status, body := b.do("POST", "/api/ai/sessions/"+b.sessID+"/proposals/"+pending[0].ID+"/confirm", nil); status != http.StatusOK {
		b.t.Fatalf("confirm: status %d\n%s", status, body)
	}
	confirmed, err := pStore.GetProposal(ctx, pending[0].ID)
	if err != nil {
		b.t.Fatalf("re-read proposal: %v", err)
	}
	return confirmed
}

// TestAIDeveloperBuildsDimensionalFormulas is AI Developer parity for member
// properties and the dimensional formulas (contract C1–C3, C9): the
// assistant declares typed properties with add_dimension_property, gives
// members their values on create_dimension and add_dimension_member, and
// saves dim.property, SUMIFS, LOOKUP and COUNTIFS metrics — all through
// propose -> confirm, no model-authoring endpoint. Promoted, its grid must
// equal, cell for cell and total for total, the grid a developer builds from
// the same definitions through the console's endpoints.
func TestAIDeveloperBuildsDimensionalFormulas(t *testing.T) {
	// ── What the assistant proposes ────────────────────────────────────────
	var steps []map[string]any
	add := func(tool, description string, params map[string]any) int {
		steps = append(steps, proposeStep(tool, description, params))
		return len(steps)
	}
	// EMEA carries its values from create_dimension, before any declaration
	// exists; US arrives with add_dimension_member after both are declared.
	region := add("create_dimension", "Create dimension 'region' with EMEA", map[string]any{
		"name": "region",
		"members": []map[string]any{
			{"code": "EMEA", "label": "EMEA", "properties": map[string]string{"segment": "SMB", "factor": "2", "peer": "US"}},
		},
	})
	add("add_dimension_property", "Declare text property 'segment'",
		map[string]any{"dimension_id": ref(region), "name": "segment", "data_type": "text"})
	add("add_dimension_property", "Declare number property 'factor'",
		map[string]any{"dimension_id": ref(region), "name": "factor", "data_type": "number"})
	add("add_dimension_property", "Declare text property 'peer'",
		map[string]any{"dimension_id": ref(region), "name": "peer", "data_type": "text"})
	add("add_dimension_member", "Add US", map[string]any{
		"dimension_id": ref(region), "code": "US", "label": "US",
		"properties": map[string]string{"segment": "ENT", "factor": "3", "peer": "EMEA"},
	})
	revenue := add("create_metric", "Input metric 'revenue'",
		map[string]any{"name": "revenue", "is_input": true, "agg_rule": "sum", "format": "number"})
	grid := add("create_grid", "Create the 'Plan' grid", map[string]any{"name": "Plan"})
	add("add_grid_dimension", "Put region on the grid", map[string]any{"grid_id": ref(grid), "dimension_id": ref(region)})
	add("add_grid_metric", "Add revenue", map[string]any{"grid_id": ref(grid), "metric_id": ref(revenue)})
	for _, m := range dimensionalParityMetrics {
		n := add("create_metric", "Calculated '"+m.name+"'", map[string]any{
			"name": m.name, "is_input": false, "formula": m.formula, "agg_rule": "sum", "format": "number",
		})
		add("add_grid_metric", "Add "+m.name, map[string]any{"grid_id": ref(grid), "metric_id": ref(n)})
	}

	b := runAIBuild(t, "give regions a segment and a factor and build the dimensional metrics", steps)
	ctx, pool, modelID, revID, do, q := context.Background(), b.pool, b.modelID, b.revID, b.do, b.q

	// The declarations landed in the AI's draft, typed, and nowhere else.
	var declared string
	if err := pool.QueryRow(ctx, `
		SELECT string_agg(p.name || ':' || p.data_type, ',' ORDER BY p.name)
		FROM model.dimension_property p JOIN model.dimension_def d ON d.id = p.dimension_id
		WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name='region'`, modelID, b.draft).Scan(&declared); err != nil {
		t.Fatalf("read declarations: %v", err)
	}
	if declared != "factor:number,peer:text,segment:text" {
		t.Errorf("draft declares %q, want factor:number,peer:text,segment:text", declared)
	}
	var inWorking int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_property p JOIN model.dimension_def d ON d.id = p.dimension_id
		WHERE d.revision_id=$1::uuid`, revID).Scan(&inWorking)
	if inWorking != 0 {
		t.Errorf("%d property declaration(s) leaked into the active revision before promotion", inWorking)
	}

	// Promote, then enter the same data the developer path enters.
	active := b.promote()
	regionID := q(`SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='region'`, modelID, active)
	revenueID := q(`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='revenue'`, modelID, active)
	gridID := q(`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Plan'`, modelID, active)
	for _, c := range []struct {
		code string
		v    float64
	}{{"US", 50}, {"EMEA", 100}} {
		if status, body := do("POST", "/api/cells", map[string]any{
			"model_id": modelID, "metric_id": revenueID, "revision_id": active,
			"dim_codes": map[string]string{regionID: c.code}, "value": c.v,
		}); status < 200 || status >= 300 {
			t.Fatalf("write revenue at %s: status %d\n%s", c.code, status, body)
		}
	}
	aiGrid := gridValuesByName(t, func(path string) (int, []byte) { return do("GET", path, nil) }, gridID, dimensionalParityWant)

	// The developer's grid, from the same definitions and data.
	devGrid := developerDimensionalGrid(t)
	for k, v := range devGrid {
		if a, ok := aiGrid[k]; !ok || !nearly(a, v) {
			t.Errorf("grid %s: AI-built %v (present %v), developer-built %v", k, a, ok, v)
		}
	}
	for k := range aiGrid {
		if _, ok := devGrid[k]; !ok {
			t.Errorf("grid %s exists only in the AI-built model", k)
		}
	}
}

// timeParityMetrics are the time-dimension formulas both paths below save:
// a dynamic LAG offset read from a member property, the YEARVALUE family,
// HALFYEARTODATE, TIMESUM over an aggregate period, START/END, and PARENT
// feeding a LOOKUP. The grid is region {World > EMEA, US} x period
// {FY26 > H1 > Q1, Q2; H2 > Q3, Q4}.
var timeParityMetrics = []struct{ name, formula string }{
	{"lagged", `LAG(revenue, region.lag, 0)`},
	{"fy", `YEARVALUE(revenue)`},
	{"hy", `HALFYEARVALUE(revenue)`},
	{"hytd", `HALFYEARTODATE(revenue)`},
	{"h2", `TIMESUM(revenue, "H2", "H2")`},
	{"months", `MONTH(END()) * 100 + MONTH(START())`},
	{"parent_rev", `IFNA(LOOKUP(revenue, region, PARENT(region)), 0)`},
}

// timeParityQuarters are the leaf periods, in order, with their dates.
var timeParityQuarters = []struct{ code, half, start, end string }{
	{"Q1", "H1", "2026-01-01", "2026-03-31"},
	{"Q2", "H1", "2026-04-01", "2026-06-30"},
	{"Q3", "H2", "2026-07-01", "2026-09-30"},
	{"Q4", "H2", "2026-10-01", "2026-12-31"},
}

// timeParityRevenue is revenue at (region, quarter n = 1..4): EMEA 10n, US
// 100n. EMEA has lag 1, US lag 2.
func timeParityRevenue(region string, n int) float64 {
	return map[string]float64{"EMEA": 10, "US": 100}[region] * float64(n)
}

// timeParityWant is a sample of what those metrics compute from that data.
var timeParityWant = map[string]float64{
	cellKey("revenue", "US", "Q4"):  400,
	cellKey("lagged", "EMEA", "Q1"): 0, cellKey("lagged", "EMEA", "Q2"): 10,
	cellKey("lagged", "US", "Q2"): 0, cellKey("lagged", "US", "Q4"): 200,
	cellKey("fy", "EMEA", "Q1"): 100, cellKey("fy", "US", "Q3"): 1000,
	cellKey("hy", "EMEA", "Q3"): 70, cellKey("hy", "US", "Q1"): 300,
	cellKey("hytd", "EMEA", "Q2"): 30, cellKey("hytd", "EMEA", "Q3"): 30, cellKey("hytd", "US", "Q4"): 700,
	cellKey("h2", "EMEA", "Q1"): 70, cellKey("h2", "US", "Q2"): 700,
	cellKey("months", "EMEA", "Q1"): 301, cellKey("months", "US", "Q4"): 1210,
	cellKey("parent_rev", "EMEA", "Q1"): 110, cellKey("parent_rev", "US", "Q4"): 440,
}

// developerTimeGrid builds the time parity model through the console's
// endpoints and returns its grid by metric name.
func developerTimeGrid(t *testing.T) map[string]float64 {
	t.Helper()
	f := setupDimFormulaFixture(t) // region {EMEA, US}, input revenue, grid "Plan"
	dims := "/api/developer/dimensions/"
	f.call("POST", dims+f.region+"/properties", map[string]any{"name": "lag", "data_type": "number"})
	world := f.call("POST", dims+f.region+"/members", map[string]any{"code": "World", "label": "World"})
	for code, lag := range map[string]string{"EMEA": "1", "US": "2"} {
		f.call("PATCH", dims+f.region+"/members/"+f.members[code], map[string]any{
			"code": code, "label": code, "parent_member_id": world, "properties": map[string]string{"lag": lag},
		})
	}
	period := f.call("POST", "/api/developer/dimensions", map[string]any{"name": "period", "revision_id": f.revID,
		"dimension_type": "time", "time_granularity": "quarter", "fiscal_year_start_month": 1})
	ids := map[string]string{}
	ids["FY26"] = f.call("POST", dims+period+"/members", map[string]any{"code": "FY26", "label": "FY26"})
	for _, h := range []string{"H1", "H2"} {
		ids[h] = f.call("POST", dims+period+"/members", map[string]any{"code": h, "label": h, "parent_member_id": ids["FY26"]})
	}
	for _, p := range timeParityQuarters {
		f.call("POST", dims+period+"/members", map[string]any{"code": p.code, "label": p.code,
			"parent_member_id": ids[p.half], "period_start": p.start, "period_end": p.end})
	}
	f.call("POST", "/api/developer/grids/"+f.gridID+"/dimensions/"+period, nil)
	for _, m := range timeParityMetrics {
		body := f.metricBody(m.name, m.formula)
		body["time_summary"] = "sum"
		f.metric[m.name] = f.call("POST", "/api/developer/metrics", body)
		f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric[m.name], nil)
	}
	for _, rg := range []string{"EMEA", "US"} {
		for i, p := range timeParityQuarters {
			f.call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "metric_id": f.metric["revenue"], "revision_id": f.revID,
				"dim_codes": map[string]string{f.region: rg, period: p.code}, "value": timeParityRevenue(rg, i+1)})
		}
	}
	return gridValuesByName(t, func(path string) (int, []byte) { return f.req("GET", path, nil) }, f.gridID, timeParityWant)
}

// TestAIDeveloperBuildsTimeDimensionalFormulas is AI Developer parity for
// the time additions and the hierarchy-aware dimensional formulas: the
// assistant creates a region hierarchy with a typed property and a quarter
// time dimension with aggregate periods, then saves a LAG whose offset is a
// member property, YEARVALUE/HALFYEARVALUE, HALFYEARTODATE, TIMESUM,
// START/END and IFNA(LOOKUP(..., PARENT(region))) — all through propose ->
// confirm. Promoted, its grid must equal, cell for cell and total for
// total, the grid a developer builds through the console's endpoints.
func TestAIDeveloperBuildsTimeDimensionalFormulas(t *testing.T) {
	var steps []map[string]any
	add := func(tool, description string, params map[string]any) int {
		steps = append(steps, proposeStep(tool, description, params))
		return len(steps)
	}
	region := add("create_dimension", "Create region: World > EMEA, US", map[string]any{
		"name": "region",
		"members": []map[string]any{
			{"code": "EMEA", "label": "EMEA"},
			{"code": "US", "label": "US"},
			{"code": "World", "label": "World"},
		},
	})
	add("add_dimension_property", "Declare number property 'lag'",
		map[string]any{"dimension_id": ref(region), "name": "lag", "data_type": "number"})
	for code, lag := range map[string]string{"EMEA": "1", "US": "2"} {
		add("update_dimension_member", "Put "+code+" under World with lag "+lag, map[string]any{
			"dimension_id": ref(region), "code": code, "parent_code": "World", "properties": map[string]string{"lag": lag},
		})
	}
	periods := []map[string]any{
		{"code": "FY26", "label": "FY26"},
		{"code": "H1", "label": "H1", "parent_code": "FY26"},
		{"code": "H2", "label": "H2", "parent_code": "FY26"},
	}
	for _, p := range timeParityQuarters {
		periods = append(periods, map[string]any{"code": p.code, "label": p.code, "parent_code": p.half,
			"period_start": p.start, "period_end": p.end})
	}
	period := add("create_dimension", "Create the quarter time dimension 'period'", map[string]any{
		"name": "period", "dimension_type": "time", "time_granularity": "quarter", "fiscal_year_start_month": 1,
		"members": periods,
	})
	revenue := add("create_metric", "Input metric 'revenue'", map[string]any{
		"name": "revenue", "is_input": true, "agg_rule": "sum", "time_summary": "sum", "format": "number",
	})
	grid := add("create_grid", "Create the 'Plan' grid", map[string]any{"name": "Plan"})
	add("add_grid_dimension", "Put region on the grid", map[string]any{"grid_id": ref(grid), "dimension_id": ref(region)})
	add("add_grid_dimension", "Put period on the grid", map[string]any{"grid_id": ref(grid), "dimension_id": ref(period)})
	add("add_grid_metric", "Add revenue", map[string]any{"grid_id": ref(grid), "metric_id": ref(revenue)})
	for _, m := range timeParityMetrics {
		n := add("create_metric", "Calculated '"+m.name+"'", map[string]any{
			"name": m.name, "is_input": false, "formula": m.formula, "agg_rule": "sum", "time_summary": "sum", "format": "number",
		})
		add("add_grid_metric", "Add "+m.name, map[string]any{"grid_id": ref(grid), "metric_id": ref(n)})
	}

	b := runAIBuild(t, "add quarters and build the time metrics", steps)
	active := b.promote()
	dimID := func(name string) string {
		return b.q(`SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`, b.modelID, active, name)
	}
	regionID, periodID := dimID("region"), dimID("period")
	revenueID := b.q(`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='revenue'`, b.modelID, active)
	gridID := b.q(`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Plan'`, b.modelID, active)
	for _, rg := range []string{"EMEA", "US"} {
		for i, p := range timeParityQuarters {
			if status, body := b.do("POST", "/api/cells", map[string]any{
				"model_id": b.modelID, "metric_id": revenueID, "revision_id": active,
				"dim_codes": map[string]string{regionID: rg, periodID: p.code}, "value": timeParityRevenue(rg, i+1),
			}); status < 200 || status >= 300 {
				t.Fatalf("write revenue at %s/%s: status %d\n%s", rg, p.code, status, body)
			}
		}
	}
	aiGrid := gridValuesByName(t, func(path string) (int, []byte) { return b.do("GET", path, nil) }, gridID, timeParityWant)

	devGrid := developerTimeGrid(t)
	for k, v := range devGrid {
		if a, ok := aiGrid[k]; !ok || !nearly(a, v) {
			t.Errorf("grid %s: AI-built %v (present %v), developer-built %v", k, a, ok, v)
		}
	}
	for k := range aiGrid {
		if _, ok := devGrid[k]; !ok {
			t.Errorf("grid %s exists only in the AI-built model", k)
		}
	}
	if len(devGrid) < 2*len(timeParityWant) {
		t.Errorf("the grids hold only %d values — the comparison is too thin to prove parity", len(devGrid))
	}
}

// TestAIDeveloperRenamesAndDeletesProperties is AI Developer parity for the
// property rename, retype and delete tools (the developer console's PATCH
// and DELETE on /api/developer/dimensions/{id}/properties/{propId}), all
// through propose -> confirm: the assistant declares 'fact' as text, then
// renames it to 'factor' and retypes it to number — the members' values
// move with it — and a formula saved against the NEW name computes after
// promotion. 'tier' is declared and deleted, after which a formula naming
// it, or the old name 'fact', is refused with UNKNOWN_PROPERTY.
func TestAIDeveloperRenamesAndDeletesProperties(t *testing.T) {
	var steps []map[string]any
	add := func(tool, description string, params map[string]any) int {
		steps = append(steps, proposeStep(tool, description, params))
		return len(steps)
	}
	region := add("create_dimension", "Create dimension 'region'", map[string]any{
		"name": "region",
		"members": []map[string]any{
			{"code": "EMEA", "label": "EMEA", "properties": map[string]string{"fact": "2", "tier": "A"}},
			{"code": "US", "label": "US", "properties": map[string]string{"fact": "3", "tier": "B"}},
		},
	})
	add("add_dimension_property", "Declare text property 'fact'",
		map[string]any{"dimension_id": ref(region), "name": "fact", "data_type": "text"})
	add("add_dimension_property", "Declare text property 'tier'",
		map[string]any{"dimension_id": ref(region), "name": "tier", "data_type": "text"})
	add("update_dimension_property", "Rename 'fact' to 'factor', typed number",
		map[string]any{"dimension_id": ref(region), "property": "fact", "name": "factor", "data_type": "number"})
	add("delete_dimension_property", "Delete property 'tier'",
		map[string]any{"dimension_id": ref(region), "property": "tier"})
	revenue := add("create_metric", "Input metric 'revenue'",
		map[string]any{"name": "revenue", "is_input": true, "agg_rule": "sum", "format": "number"})
	grid := add("create_grid", "Create the 'Plan' grid", map[string]any{"name": "Plan"})
	add("add_grid_dimension", "Put region on the grid", map[string]any{"grid_id": ref(grid), "dimension_id": ref(region)})
	add("add_grid_metric", "Add revenue", map[string]any{"grid_id": ref(grid), "metric_id": ref(revenue)})
	scaled := add("create_metric", "Calculated 'scaled' = revenue * region.factor", map[string]any{
		"name": "scaled", "is_input": false, "formula": "revenue * region.factor", "agg_rule": "sum", "format": "number",
	})
	add("add_grid_metric", "Add scaled", map[string]any{"grid_id": ref(grid), "metric_id": ref(scaled)})

	b := runAIBuild(t, "rename the region factor property, make it a number, drop tier, and scale revenue by it", steps)
	ctx, pool, modelID, do := context.Background(), b.pool, b.modelID, b.do

	// The draft declares only factor, typed number, and the members' values
	// moved to the new name.
	declared := b.q(`
		SELECT string_agg(p.name || ':' || p.data_type, ',' ORDER BY p.name)
		FROM model.dimension_property p JOIN model.dimension_def d ON d.id = p.dimension_id
		WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name='region'`, modelID, b.draft)
	if declared != "factor:number" {
		t.Errorf("draft declares %q, want factor:number", declared)
	}
	emea := b.q(`SELECT m.properties::text FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
		WHERE d.revision_id=$1::uuid AND d.name='region' AND m.code='EMEA'`, b.draft)
	if emea != `{"tier": "A", "factor": "2"}` {
		t.Errorf("EMEA properties %s, want the value under factor (tier's value stays stored, undeclared)", emea)
	}

	// Promoted, the formula written against the new name computes.
	active := b.promote()
	regionID := b.q(`SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='region'`, modelID, active)
	revenueID := b.q(`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='revenue'`, modelID, active)
	gridID := b.q(`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Plan'`, modelID, active)
	for code, v := range map[string]float64{"EMEA": 100, "US": 50} {
		if status, body := do("POST", "/api/cells", map[string]any{
			"model_id": modelID, "metric_id": revenueID, "revision_id": active,
			"dim_codes": map[string]string{regionID: code}, "value": v,
		}); status < 200 || status >= 300 {
			t.Fatalf("write revenue at %s: status %d\n%s", code, status, body)
		}
	}
	gridValuesByName(t, func(path string) (int, []byte) { return do("GET", path, nil) }, gridID, map[string]float64{
		cellKey("scaled", "EMEA"): 200, cellKey("scaled", "US"): 150,
	})

	// The deleted property, and the old name, are refused by the next
	// proposal with the developer path's code.
	refused := b.proposeAgain("add tiered revenue and a metric on the old fact property", []map[string]any{
		proposeStep("create_metric", "Calculated 'tiered'", map[string]any{
			"name": "tiered", "is_input": false, "formula": `IF(region.tier = "A", revenue, 0)`, "format": "number",
		}),
		proposeStep("create_metric", "Calculated 'stale'", map[string]any{
			"name": "stale", "is_input": false, "formula": "revenue * region.fact", "format": "number",
		}),
	})
	if refused.Status != "partial" {
		t.Errorf("second proposal finished %q, want partial", refused.Status)
	}
	for _, s := range refused.Steps {
		if s.Status != "failed" || !strings.Contains(s.Result, "UNKNOWN_PROPERTY") {
			t.Errorf("step %q: %s %q, want failed with UNKNOWN_PROPERTY", s.Description, s.Status, s.Result)
		}
	}
	var saved int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid AND name IN ('tiered','stale')`, modelID).Scan(&saved)
	if saved != 0 {
		t.Errorf("%d metric(s) naming a deleted or renamed-away property were saved", saved)
	}
}
