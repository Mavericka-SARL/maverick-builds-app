// cmd/seed-sandbox — a small, realistic planning model for exercising a REAL
// deployment.
//
// Every other seeder in this repo authenticates with the X-Dev-User header,
// which only resolves when DEV_MODE=true. A production gateway refuses it, so
// none of them can run against a real deployment. This one sends a bearer
// token instead.
//
// It also creates no users, deliberately. Since user creation began
// provisioning real identity-provider accounts and emailing invitations, a
// seeder that invents demo addresses would fail at the invitation step (and
// undo itself) — the demo domains do not accept mail.
//
// Everything lands in its own application, named by APP_NAME, so it can be
// removed in one action without touching anything else in the tenant.
//
// Usage:
//
//	GATEWAY_URL=https://example.app \
//	MAVERICKS_TOKEN=<platform_admin access token> \
//	TENANT="Acme" \
//	go run ./cmd/seed-sandbox
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// ── tiny HTTP client ──────────────────────────────────────────────────────

type api struct {
	base       string
	token      string
	client     *http.Client
	appID      string
	modelID    string
	revisionID string
}

func (a *api) withApp(id string) *api   { n := *a; n.appID = id; return &n }
func (a *api) withModel(id string) *api { n := *a; n.modelID = id; return &n }
func (a *api) withRev(id string) *api   { n := *a; n.revisionID = id; return &n }

func (a *api) raw(method, path string, body any) ([]byte, int) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			log.Fatalf("marshal %s %s: %v", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, a.base+path, rdr)
	if err != nil {
		log.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)
	if a.appID != "" {
		req.Header.Set("X-App-Id", a.appID)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		log.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode
}

func (a *api) call(method, path string, body any) map[string]any {
	raw, status := a.raw(method, path, body)
	if status >= 300 {
		log.Fatalf("%s %s -> %d: %s", method, path, status, raw)
	}
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		log.Fatalf("%s %s: decode: %v (body=%s)", method, path, err, raw)
	}
	return m
}

func (a *api) list(method, path string) []map[string]any {
	raw, status := a.raw(method, path, nil)
	if status >= 300 {
		log.Fatalf("%s %s -> %d: %s", method, path, status, raw)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatalf("%s %s: decode list: %v (body=%s)", method, path, err, raw)
	}
	return out
}

func id(m map[string]any) string {
	s, _ := m["id"].(string)
	if s == "" {
		log.Fatalf("expected an \"id\" in response, got %+v", m)
	}
	return s
}

func step(format string, args ...any) {
	fmt.Printf("\033[36m▸\033[0m %s\n", fmt.Sprintf(format, args...))
}

// ── the model ─────────────────────────────────────────────────────────────

var departments = []struct{ code, label string }{
	{"ENG", "Engineering"},
	{"SLS", "Sales"},
	{"MKT", "Marketing"},
	{"OPS", "Operations"},
}

var months = []struct{ code, label string }{
	{"2026-01", "Jan 2026"},
	{"2026-02", "Feb 2026"},
	{"2026-03", "Mar 2026"},
	{"2026-04", "Apr 2026"},
	{"2026-05", "May 2026"},
	{"2026-06", "Jun 2026"},
}

// Plausible starting points per department, varied per month below so the
// grid has movement in it rather than one repeated number.
var seedByDept = map[string]struct {
	headcount   float64
	avgSalary   float64
	travel      float64
	targetTotal float64
}{
	"ENG": {headcount: 24, avgSalary: 9500, travel: 8000, targetTotal: 245000},
	"SLS": {headcount: 14, avgSalary: 7800, travel: 22000, targetTotal: 135000},
	"MKT": {headcount: 8, avgSalary: 7200, travel: 12000, targetTotal: 72000},
	"OPS": {headcount: 11, avgSalary: 6400, travel: 5000, targetTotal: 78000},
}

func main() {
	base := os.Getenv("GATEWAY_URL")
	if base == "" {
		log.Fatal("set GATEWAY_URL, e.g. https://example.app")
	}
	token := os.Getenv("MAVERICKS_TOKEN")
	if token == "" {
		log.Fatal("set MAVERICKS_TOKEN to a platform_admin access token")
	}
	tenantName := os.Getenv("TENANT")
	if tenantName == "" {
		log.Fatal("set TENANT to the name of the tenant to seed")
	}
	appName := os.Getenv("APP_NAME")
	if appName == "" {
		appName = "Sandbox"
	}

	root := &api{base: base, token: token, client: &http.Client{Timeout: 120 * time.Second}}

	me := root.call("GET", "/api/me", nil)
	step("authenticated as %v (%v)", me["email"], me["roles"])

	// Find the tenant rather than creating one: this runs against a real
	// deployment where the tenant already exists and belongs to someone.
	var customerID string
	for _, t := range root.list("GET", "/api/admin/tenants") {
		if t["name"] == tenantName {
			customerID, _ = t["id"].(string)
		}
	}
	if customerID == "" {
		log.Fatalf("tenant %q not found — set TENANT to an existing one", tenantName)
	}
	step("tenant %q", tenantName)

	step("creating application %q", appName)
	appID := id(root.call("POST", "/api/admin/applications", map[string]any{
		"customer_id": customerID, "name": appName, "mode": "planning",
	}))

	modelID := id(root.call("POST", "/api/admin/models", map[string]any{
		"application_id": appID, "name": "Operating Plan", "storage_type": "oltp",
	}))
	revID := id(root.call("POST", "/api/admin/revisions", map[string]any{
		"model_id": modelID, "name": "Working", "description": "Sandbox working revision",
	}))
	// A model with no active revision is not usable — creating the revision
	// does not select it.
	root.call("PUT", fmt.Sprintf("/api/admin/models/%s/active-revision", modelID),
		map[string]any{"revision_name": "Working"})
	step("application/model/revision ready (revision active)")

	dev := root.withApp(appID).withModel(modelID).withRev(revID)

	step("creating dimensions")
	deptDim := id(dev.call("POST", "/api/developer/dimensions", map[string]any{
		"name": "Department", "agg_rule": "sum", "revision_id": revID,
	}))
	monthDim := id(dev.call("POST", "/api/developer/dimensions", map[string]any{
		"name": "Month", "agg_rule": "sum", "revision_id": revID,
	}))
	for _, d := range departments {
		dev.call("POST", fmt.Sprintf("/api/developer/dimensions/%s/members", deptDim),
			map[string]any{"code": d.code, "label": d.label})
	}
	for _, m := range months {
		dev.call("POST", fmt.Sprintf("/api/developer/dimensions/%s/members", monthDim),
			map[string]any{"code": m.code, "label": m.label})
	}
	step("  %d departments x %d months", len(departments), len(months))

	step("creating metrics")
	input := func(name string) string {
		return id(dev.call("POST", "/api/developer/metrics", map[string]any{
			"name": name, "is_input": true, "revision_id": revID,
		}))
	}
	calc := func(name, formula, aggRule string) string {
		body := map[string]any{
			"name": name, "is_input": false, "formula": formula, "revision_id": revID,
		}
		if aggRule != "" {
			body["agg_rule"] = aggRule
		}
		return id(dev.call("POST", "/api/developer/metrics", body))
	}
	headcount := input("headcount")
	avgSalary := input("avg_salary")
	travel := input("travel_cost")
	target := input("budget_target")

	salaryCost := calc("salary_cost", "=headcount*avg_salary", "")
	totalCost := calc("total_cost", "=headcount*avg_salary+travel_cost", "")
	variance := calc("variance", "=budget_target-(headcount*avg_salary+travel_cost)", "")
	// IFERROR guards the month deliberately left without a target below;
	// without it that division by zero fails the whole metric, not one cell.
	//
	// agg_rule stays the default sum. average looks like the right choice for
	// a percentage and is a trap here: it switches the scheduler to a
	// total-level shortcut that evaluates the formula ONCE against aggregated
	// inputs, instead of per intersection. For a formula containing a product
	// of two inputs that is simply wrong — sum(headcount) * sum(avg_salary)
	// is not sum(headcount * avg_salary) — and it produced -1936.77% here,
	// with zero per-cell rows, while sum gives correct per-cell values.
	//
	// So the per-cell numbers are right and the whole-model rollup is a sum
	// of percentages, which is not a meaningful figure. Getting that right
	// needs total_variance/total_target as its own metric rather than an
	// aggregation rule over a ratio.
	variancePct := calc("variance_pct",
		"=IFERROR((budget_target-(headcount*avg_salary+travel_cost))/budget_target*100,0)", "")
	step("  4 input metrics, 4 calculated")

	// The grid is what tells the calculation engine which intersections a
	// metric spans. Without it the scheduler evaluates each formula once at
	// whole-model scope, writes a single calc_result with empty dim_members,
	// and every calculated metric reads as 0 — the inputs are all stored per
	// department and month, so nothing matches at that scope.
	step("creating grid \"Operating Plan\" and assigning dimensions + metrics")
	gridID := id(dev.call("POST", "/api/developer/grids", map[string]any{
		"name": "Operating Plan", "revision_id": revID,
	}))
	for _, dimID := range []string{deptDim, monthDim} {
		dev.call("POST", fmt.Sprintf("/api/developer/grids/%s/dimensions/%s", gridID, dimID), nil)
	}
	for _, metricID := range []string{
		headcount, avgSalary, travel, target,
		salaryCost, totalCost, variance, variancePct,
	} {
		dev.call("POST", fmt.Sprintf("/api/developer/grids/%s/metrics/%s", gridID, metricID), nil)
	}

	step("writing cells (each write triggers a synchronous recalculation)")
	t0 := time.Now()
	n := 0
	for _, d := range departments {
		s := seedByDept[d.code]
		for i, m := range months {
			// A gentle ramp plus a per-department wobble, so charts and
			// variances have something to show.
			growth := 1 + float64(i)*0.02
			wobble := 1 + float64((i*7+len(d.code))%5)*0.01

			// dim_codes is keyed by dimension ID, not dimension name.
			// Names are accepted and stored verbatim, so the writes appear to
			// succeed and the facts land — they simply never line up with any
			// dimension, and every calculated metric silently reads 0.
			dimCodes := map[string]string{deptDim: d.code, monthDim: m.code}
			write := func(metricID string, value float64) {
				dev.call("POST", "/api/cells", map[string]any{
					"model_id": modelID, "metric_id": metricID, "revision_id": revID,
					"dim_codes": dimCodes,
					"value":     value,
				})
				n++
			}
			write(headcount, float64(int(s.headcount*growth)))
			write(avgSalary, float64(int(s.avgSalary*wobble)))
			write(travel, float64(int(s.travel*wobble)))
			// One month deliberately left without a target, to exercise the
			// IFERROR path above rather than assuming it works.
			if d.code != "MKT" || m.code != "2026-06" {
				write(target, float64(int(s.targetTotal*growth)))
			}
		}
	}
	step("  %d cells in %s", n, time.Since(t0).Round(time.Millisecond))

	grid := dev.call("GET", fmt.Sprintf("/api/grid?revision_id=%s", revID), nil)
	totals, _ := grid["totals"].(map[string]any)
	step("grid returns %d metric totals", len(totals))

	fmt.Printf("\n\033[1mSandbox ready\033[0m\n")
	fmt.Printf("  tenant       %s\n", tenantName)
	fmt.Printf("  application  %s (planning)\n", appName)
	fmt.Printf("  model        Operating Plan, revision Working (active)\n")
	fmt.Printf("  dimensions   Department (%d), Month (%d)\n", len(departments), len(months))
	fmt.Printf("  metrics      4 input, 4 calculated\n")
	fmt.Printf("  cells        %d\n", n)
	fmt.Printf("\n  Remove it all by deleting the %q application.\n", appName)
}
