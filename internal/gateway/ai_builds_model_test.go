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
	"strings"
	"testing"

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
	buildDim("period", []node{
		{"FY26", "FY26", ""},
		{"H1", "H1", "FY26"}, {"H2", "H2", "FY26"},
		{"Q1", "Q1", "H1"}, {"Q2", "Q2", "H1"},
		{"Q3", "Q3", "H2"}, {"Q4", "Q4", "H2"},
	})

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

	// The forecast: three more inputs and the 24 calculated metrics that
	// exercise 22 formula functions between them. This is the part the old
	// hand-rolled validator could not have built at all — every one of these
	// formulas carries either a function call or a {reference}, and it read
	// both as missing metric names.
	for _, in := range []string{"prior_revenue", "seasonality", "pipeline"} {
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
	wantMetrics := len(salesdemo.MetricNames) + 3 + len(salesdemo.ForecastMetrics)
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
		"prior_revenue": "sum", "seasonality": "sum", "pipeline": "sum",
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
