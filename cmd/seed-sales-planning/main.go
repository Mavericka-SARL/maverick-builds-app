// cmd/seed-sales-planning — creates the sales-planning demo model in a real
// deployment.
//
// The model itself comes from internal/salesdemo, which is also what the
// integration test in internal/gateway builds. That is the point: a demo whose
// seeded shape had drifted from its tested shape would be worse than no demo,
// because the tests would keep passing against a model nobody has.
//
// It authenticates with a bearer token rather than the X-Dev-User header every
// other seeder uses, because that header only resolves when DEV_MODE=true and
// a production gateway refuses it.
//
// Users are NOT created. Since user creation began provisioning real
// identity-provider accounts and emailing invitations, inventing demo
// addresses fails at the invitation step and rolls the user back — the demo
// domains do not accept mail. Invite real people from the console and give
// them access rules there; the model does not depend on it.
//
// Everything lands in its own application so it can be removed in one action.
//
// Usage:
//
//	GATEWAY_URL=https://example.app \
//	MAVERICKS_TOKEN=<platform_admin access token> \
//	TENANT="Acme" \
//	APP_NAME="Test app" \
//	go run ./cmd/seed-sales-planning
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

	"github.com/mavericks-engine/mavericks/internal/salesdemo"
)

type api struct {
	base, token string
	appID       string
	client      *http.Client
}

func (a *api) withApp(id string) *api { n := *a; n.appID = id; return &n }

// call is salesdemo.Caller: it returns the error rather than exiting, so a
// failure part-way through names the step that failed instead of a bare stack.
func (a *api) call(method, path string, body any) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, a.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)
	if a.appID != "" {
		req.Header.Set("X-App-Id", a.appID)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s -> %d: %s", method, path, resp.StatusCode, raw)
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w (body=%s)", method, path, err, raw)
	}
	return out, nil
}

// must is for the bootstrap steps, where there is nothing useful to do with a
// failure except say which one it was.
func (a *api) must(method, path string, body any) map[string]any {
	out, err := a.call(method, path, body)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return out
}

func (a *api) list(path string) []map[string]any {
	var rdr io.Reader
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, a.base+path, rdr)
	if err != nil {
		log.Fatalf("build GET %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.client.Do(req)
	if err != nil {
		log.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		log.Fatalf("GET %s -> %d: %s", path, resp.StatusCode, raw)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatalf("decode GET %s: %v", path, err)
	}
	return out
}

func id(m map[string]any) string { v, _ := m["id"].(string); return v }

func step(format string, args ...any) { log.Printf("→ "+format, args...) }

func main() {
	base := os.Getenv("GATEWAY_URL")
	if base == "" {
		log.Fatal("set GATEWAY_URL, e.g. https://console.example.com")
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
		appName = "Test app"
	}

	root := &api{base: base, token: token, client: &http.Client{Timeout: 120 * time.Second}}

	me := root.must("GET", "/api/me", nil)
	step("authenticated as %v %v", me["email"], me["roles"])

	// Find the tenant rather than create one: this runs against a deployment
	// where the tenant already exists and belongs to someone.
	var customerID string
	for _, t := range root.list("/api/admin/tenants") {
		if t["name"] == tenantName {
			customerID, _ = t["id"].(string)
		}
	}
	if customerID == "" {
		log.Fatalf("tenant %q not found — set TENANT to one that exists", tenantName)
	}
	step("tenant %q", tenantName)

	appID := id(root.must("POST", "/api/admin/applications", map[string]any{
		"customer_id": customerID, "name": appName, "mode": "planning",
	}))
	modelID := id(root.must("POST", "/api/admin/models", map[string]any{
		"application_id": appID, "name": "Sales Planning", "storage_type": "oltp",
	}))
	revID := id(root.must("POST", "/api/admin/revisions", map[string]any{
		"model_id": modelID, "name": "Working", "description": "Sales planning working revision",
	}))
	// Creating a revision does not select it, and a model with no active
	// revision is not usable.
	root.must("PUT", fmt.Sprintf("/api/admin/models/%s/active-revision", modelID),
		map[string]any{"revision_name": "Working"})
	step("application %q / model / revision ready", appName)

	dev := root.withApp(appID)

	step("building dimensions, metrics and the grid")
	model, err := salesdemo.Build(dev.call, revID)
	if err != nil {
		log.Fatalf("build model: %v", err)
	}
	step("3 dimensions x 3 levels, %d metrics, 1 grid", len(salesdemo.MetricNames))

	step("adding the quarterly forecast")
	if err := salesdemo.BuildForecast(dev.call, revID, model); err != nil {
		log.Fatalf("build forecast: %v", err)
	}
	step("%d forecast metrics covering 22 formula functions", len(salesdemo.ForecastMetricNames))

	step("writing the sample plan")
	if err := model.WriteFacts(dev.call, modelID, revID, salesdemo.SampleFacts()); err != nil {
		log.Fatalf("write facts: %v", err)
	}
	if err := model.WriteForecastFacts(dev.call, modelID, revID, salesdemo.SampleForecastFacts()); err != nil {
		log.Fatalf("write forecast inputs: %v", err)
	}

	step("done")
	fmt.Printf(`
Sales Planning is ready in %q.

  application  %s
  model        %s
  revision     %s
  grid         %s

The plan is deliberately lopsided — the UK sells many cheap laptops, Germany a
few expensive ones — so the aggregation rules disagree with each other and you
can see which is which:

  margin_pct   agg_rule "formula"  total is the ratio of the sums, not a sum
                                   or a mean of the per-member percentages
  avg_price    agg_rule "rate"     total is revenue over units, so the
                                   high-volume line carries the weight

"period" is a declared time dimension (four fiscal quarters of 2026), so the
forecast reads the previous quarter directly — growth_pct uses
LAG(revenue, 1, 0) and cagr_pct uses PREVIOUS(revenue) — instead of a
separately maintained prior-period input.

No users were created; invite real people from the console and set their
access rules there.
`, appName, appID, modelID, revID, model.GridID)
}
