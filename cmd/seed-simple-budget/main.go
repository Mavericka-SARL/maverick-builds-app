// cmd/seed-simple-budget creates and verifies a minimal budgeting model
// through the same HTTP API the console uses.
//
// The command deliberately creates an isolated application. It never writes
// directly to PostgreSQL and it never deletes an application it did not create.
//
// Production usage (one identity may be used for both variables only when it
// has both an admin role and the developer role):
//
//	GATEWAY_URL=https://console.example.com \
//	MAVERICKS_ADMIN_TOKEN=<short-lived-admin-token> \
//	MAVERICKS_DEVELOPER_TOKEN=<short-lived-developer-token> \
//	TENANT="Acme" \
//	go run ./cmd/seed-simple-budget
//
// Local DEV_MODE usage after running a normal repository seed (go run ./cmd/seed):
//
//	go run ./cmd/seed-simple-budget
//
// Optional environment:
//
//	TENANT     tenant (customer) name. Optional when the developer can see
//	           exactly one tenant; required otherwise.
//	APP_NAME   application name (default: "Simple Budget Tutorial <UTC stamp>").
//	CLEANUP=1  delete the application after a successful verification.
//	TRACE=1    print every HTTP call and its status code.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	modelName    = "Simple Budget Model"
	revisionName = "Working"
	gridName     = "Budget vs Actual"
)

type credentials struct {
	token   string
	devUser string
}

type api struct {
	base   string
	client *http.Client
	creds  credentials
	appID  string
	trace  bool
}

func (a *api) withApp(id string) *api {
	next := *a
	next.appID = id
	return &next
}

// raw performs one HTTP call and returns the body and status without judging
// the status. Callers that expect a failure (the read-only check) use it
// directly; everything else goes through call/list, which stop on any error.
func (a *api) raw(method, path string, body any) ([]byte, int) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			log.Fatalf("marshal %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, a.base+path, reader)
	if err != nil {
		log.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.creds.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.creds.token)
	}
	if a.creds.devUser != "" {
		req.Header.Set("X-Dev-User", a.creds.devUser)
	}
	if a.appID != "" {
		req.Header.Set("X-App-Id", a.appID)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		log.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	payload, _ := io.ReadAll(resp.Body)
	if a.trace {
		who := a.creds.devUser
		if who == "" {
			who = "token"
		}
		fmt.Printf("    %-6s %-60s -> %d  [%s]\n", method, path, resp.StatusCode, who)
	}
	return payload, resp.StatusCode
}

func (a *api) call(method, path string, body any) map[string]any {
	payload, status := a.raw(method, path, body)
	if status >= http.StatusMultipleChoices {
		log.Fatalf("%s %s -> %d: %s", method, path, status, payload)
	}
	if len(payload) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		log.Fatalf("decode %s %s: %v (body=%s)", method, path, err, payload)
	}
	return out
}

func (a *api) list(path string) []map[string]any {
	payload, status := a.raw(http.MethodGet, path, nil)
	if status >= http.StatusMultipleChoices {
		log.Fatalf("GET %s -> %d: %s", path, status, payload)
	}
	var out []map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		log.Fatalf("decode GET %s: %v (body=%s)", path, err, payload)
	}
	return out
}

func responseID(m map[string]any) string {
	value, _ := m["id"].(string)
	if value == "" {
		log.Fatalf("expected an id in response, got %+v", m)
	}
	return value
}

func roles(m map[string]any) []string {
	raw, _ := m["roles"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if role, ok := item.(string); ok && !slices.Contains(out, role) {
			out = append(out, role)
		}
	}
	return out
}

func requireRole(me map[string]any, allowed ...string) {
	have := roles(me)
	for _, role := range allowed {
		if slices.Contains(have, role) {
			return
		}
	}
	log.Fatalf("%v must include one of roles %v", have, allowed)
}

// tenantID returns the id of the tenant named name from a tenant listing, or
// "" when it is absent. Both /api/admin/tenants and /api/developer/applications
// return the same shape, each filtered to what the caller may see.
func tenantID(tenants []map[string]any, name string) string {
	for _, tenant := range tenants {
		if tenant["name"] == name {
			id, _ := tenant["id"].(string)
			return id
		}
	}
	return ""
}

func tenantNames(tenants []map[string]any) []string {
	out := make([]string, 0, len(tenants))
	for _, tenant := range tenants {
		name, _ := tenant["name"].(string)
		out = append(out, name)
	}
	return out
}

// check is one named assertion. All checks in a run are counted so the final
// line can state how many passed; the first failure stops the run with exit 1.
var checks int

func assertNumber(label string, values map[string]any, key string, want float64) {
	got, ok := values[key].(float64)
	if !ok {
		log.Fatalf("ASSERT FAIL: %s (key %q) is absent or not numeric (value=%v)", label, key, values[key])
	}
	if math.Abs(got-want) > 0.000001 {
		log.Fatalf("ASSERT FAIL: %s = %.2f; want %.2f", label, got, want)
	}
	checks++
	fmt.Printf("  PASS  %-34s %12.2f\n", label, got)
}

func assertMetricDefinition(model map[string]any, metricID, formula string, dependencies ...string) {
	metrics, _ := model["metrics"].([]any)
	for _, raw := range metrics {
		metric, _ := raw.(map[string]any)
		if metric["id"] != metricID {
			continue
		}
		if metric["formula"] != formula {
			log.Fatalf("ASSERT FAIL: formula = %v; want %q", metric["formula"], formula)
		}
		var gotDeps []string
		rawDeps, _ := metric["depends_on"].([]any)
		for _, dep := range rawDeps {
			if name, ok := dep.(string); ok {
				gotDeps = append(gotDeps, name)
			}
		}
		slices.Sort(gotDeps)
		wantDeps := append([]string(nil), dependencies...)
		slices.Sort(wantDeps)
		if !slices.Equal(gotDeps, wantDeps) {
			log.Fatalf("ASSERT FAIL: dependencies = %v; want %v", gotDeps, wantDeps)
		}
		checks++
		fmt.Printf("  PASS  %-34s %s -> %v\n", "formula and dependency graph", formula, gotDeps)
		return
	}
	log.Fatalf("ASSERT FAIL: metric %s not returned by developer model endpoint", metricID)
}

func isLoopback(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func step(format string, args ...any) {
	fmt.Printf("\033[36m>\033[0m %s\n", fmt.Sprintf(format, args...))
}

func main() {
	base := strings.TrimRight(os.Getenv("GATEWAY_URL"), "/")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	trace := os.Getenv("TRACE") != "" && os.Getenv("TRACE") != "0"
	cleanup := os.Getenv("CLEANUP") != "" && os.Getenv("CLEANUP") != "0"

	sharedToken := os.Getenv("MAVERICKS_TOKEN")
	adminCreds := credentials{
		token:   firstNonEmpty(os.Getenv("MAVERICKS_ADMIN_TOKEN"), sharedToken),
		devUser: os.Getenv("MAVERICKS_ADMIN_DEV_USER"),
	}
	developerCreds := credentials{
		token:   firstNonEmpty(os.Getenv("MAVERICKS_DEVELOPER_TOKEN"), sharedToken),
		devUser: os.Getenv("MAVERICKS_DEVELOPER_DEV_USER"),
	}

	if isLoopback(base) {
		if adminCreds.token == "" && adminCreds.devUser == "" {
			adminCreds.devUser = "platform_admin"
		}
		if developerCreds.token == "" && developerCreds.devUser == "" {
			developerCreds.devUser = "developer"
		}
	} else if adminCreds.token == "" || developerCreds.token == "" {
		log.Fatal("a non-loopback GATEWAY_URL requires MAVERICKS_ADMIN_TOKEN and MAVERICKS_DEVELOPER_TOKEN (or one MAVERICKS_TOKEN whose identity has both roles)")
	}

	appName := os.Getenv("APP_NAME")
	if appName == "" {
		appName = "Simple Budget Tutorial " + time.Now().UTC().Format("20060102-150405")
	}

	client := &http.Client{Timeout: 120 * time.Second}
	admin := &api{base: base, client: client, creds: adminCreds, trace: trace}
	developer := &api{base: base, client: client, creds: developerCreds, trace: trace}

	// ── 1. Who is calling ─────────────────────────────────────────────────
	adminMe := admin.call(http.MethodGet, "/api/me", nil)
	requireRole(adminMe, "platform_admin", "tenant_admin")
	step("admin authenticated as %v %v", adminMe["email"], roles(adminMe))

	developerMe := developer.call(http.MethodGet, "/api/me", nil)
	requireRole(developerMe, "developer")
	step("developer authenticated as %v %v", developerMe["email"], roles(developerMe))

	// ── 2. Pick the tenant BEFORE creating anything ───────────────────────
	// The developer role is scoped to a tenant. An application created by the
	// admin in a tenant the developer cannot see would be unusable by the
	// developer (every developer call returns 403 "outside your access
	// scope"), so the tenant is chosen from what the developer can see and
	// then confirmed against what the admin can see.
	developerTenants := developer.list("/api/developer/applications")
	tenantName := os.Getenv("TENANT")
	if tenantName == "" {
		switch len(developerTenants) {
		case 1:
			tenantName, _ = developerTenants[0]["name"].(string)
		case 0:
			log.Fatal("the developer identity can see no tenant; grant it access to one first")
		default:
			log.Fatalf("the developer identity can see %d tenants %v; set TENANT to one of them",
				len(developerTenants), tenantNames(developerTenants))
		}
	}
	customerID := tenantID(developerTenants, tenantName)
	if customerID == "" {
		log.Fatalf("tenant %q is not visible to the developer identity (it sees %v); nothing was created",
			tenantName, tenantNames(developerTenants))
	}
	if tenantID(admin.list("/api/admin/tenants"), tenantName) != customerID {
		log.Fatalf("tenant %q is not visible to the admin identity; nothing was created", tenantName)
	}
	step("using tenant %q (%s)", tenantName, customerID)

	// ── 3. Admin: application → model → revision → activate ───────────────
	appID := responseID(admin.call(http.MethodPost, "/api/admin/applications", map[string]any{
		"customer_id": customerID,
		"name":        appName,
		"mode":        "planning",
	}))
	modelID := responseID(admin.call(http.MethodPost, "/api/admin/models", map[string]any{
		"application_id": appID,
		"name":           modelName,
		"storage_type":   "oltp",
	}))
	revisionID := responseID(admin.call(http.MethodPost, "/api/admin/revisions", map[string]any{
		"model_id":    modelID,
		"name":        revisionName,
		"description": "Minimal budget-versus-actual tutorial",
	}))
	// Creating a revision does not activate it. A model without an active
	// revision is not usable by anyone.
	admin.call(http.MethodPut, fmt.Sprintf("/api/admin/models/%s/active-revision", modelID), map[string]any{
		"revision_name": revisionName,
	})
	step("created application %q, model %q, revision %q (active)", appName, modelName, revisionName)

	// ── 4. Developer: dimension, metrics, grid ────────────────────────────
	// Every developer call carries X-App-Id so the server resolves the
	// application context; set-default marks the model as the app's default.
	dev := developer.withApp(appID)
	dev.call(http.MethodPost, fmt.Sprintf("/api/developer/models/%s/set-default", modelID), nil)

	departmentID := responseID(dev.call(http.MethodPost, "/api/developer/dimensions", map[string]any{
		"name": "Department", "agg_rule": "sum", "revision_id": revisionID,
	}))
	for _, department := range []struct{ code, label string }{
		{"ENG", "Engineering"},
		{"SALES", "Sales"},
	} {
		dev.call(http.MethodPost, fmt.Sprintf("/api/developer/dimensions/%s/members", departmentID), map[string]any{
			"code": department.code, "label": department.label,
		})
	}
	step("created dimension Department with members ENG, SALES")

	inputMetric := func(name string) string {
		return responseID(dev.call(http.MethodPost, "/api/developer/metrics", map[string]any{
			"name": name, "is_input": true, "revision_id": revisionID,
			"agg_rule": "sum", "format": "currency", "format_decimals": 0, "format_currency": "$",
		}))
	}
	budgetID := inputMetric("budget")
	actualID := inputMetric("actual")
	varianceFormula := "=budget-actual"
	varianceID := responseID(dev.call(http.MethodPost, "/api/developer/metrics", map[string]any{
		"name": "variance", "is_input": false, "formula": varianceFormula, "revision_id": revisionID,
		"agg_rule": "sum", "format": "currency", "format_decimals": 0, "format_currency": "$",
	}))
	step("created input metrics budget, actual and calculated metric variance %s", varianceFormula)

	gridID := responseID(dev.call(http.MethodPost, "/api/developer/grids", map[string]any{
		"name": gridName, "revision_id": revisionID,
	}))
	dev.call(http.MethodPost, fmt.Sprintf("/api/developer/grids/%s/dimensions/%s", gridID, departmentID), nil)
	for _, metricID := range []string{budgetID, actualID, varianceID} {
		dev.call(http.MethodPost, fmt.Sprintf("/api/developer/grids/%s/metrics/%s", gridID, metricID), nil)
	}
	step("created grid %q with one dimension and three metrics", gridName)

	// ── 5. Data: four input cells ─────────────────────────────────────────
	write := func(metricID, department string, value float64) {
		dev.call(http.MethodPost, "/api/cells", map[string]any{
			"model_id": modelID, "revision_id": revisionID, "metric_id": metricID,
			"dim_codes": map[string]string{departmentID: department}, "value": value,
		})
	}
	write(budgetID, "ENG", 100000)
	write(actualID, "ENG", 65000)
	write(budgetID, "SALES", 80000)
	write(actualID, "SALES", 55000)
	step("wrote four input cells; each write synchronously recalculated variance")

	readGrid := func() (cells, totals map[string]any) {
		grid := dev.call(http.MethodGet,
			fmt.Sprintf("/api/grid?grid_def_id=%s&revision_id=%s", gridID, revisionID), nil)
		cells, _ = grid["cells"].(map[string]any)
		totals, _ = grid["totals"].(map[string]any)
		return cells, totals
	}

	// ── 6. Verification ───────────────────────────────────────────────────
	fmt.Println("\nVerification 1: values and calculation")
	cells, totals := readGrid()
	assertNumber("budget @ ENG", cells, budgetID+":ENG", 100000)
	assertNumber("actual @ ENG", cells, actualID+":ENG", 65000)
	assertNumber("variance @ ENG (calculated)", cells, varianceID+":ENG", 35000)
	assertNumber("budget @ SALES", cells, budgetID+":SALES", 80000)
	assertNumber("actual @ SALES", cells, actualID+":SALES", 55000)
	assertNumber("variance @ SALES (calculated)", cells, varianceID+":SALES", 25000)
	assertNumber("budget total (sum rollup)", totals, budgetID, 180000)
	assertNumber("actual total (sum rollup)", totals, actualID, 120000)
	assertNumber("variance total (sum rollup)", totals, varianceID, 60000)

	fmt.Println("\nVerification 2: definition as the developer sees it")
	model := dev.call(http.MethodGet, "/api/developer/model?revision_id="+revisionID, nil)
	assertMetricDefinition(model, varianceID, varianceFormula, "actual", "budget")

	fmt.Println("\nVerification 3: an update recalculates dependents")
	write(actualID, "ENG", 70000)
	cells, totals = readGrid()
	assertNumber("actual @ ENG after update", cells, actualID+":ENG", 70000)
	assertNumber("variance @ ENG after update", cells, varianceID+":ENG", 30000)
	assertNumber("variance @ SALES unchanged", cells, varianceID+":SALES", 25000)
	assertNumber("variance total after update", totals, varianceID, 55000)

	fmt.Println("\nVerification 4: a calculated metric rejects input")
	_, status := dev.raw(http.MethodPost, "/api/cells", map[string]any{
		"model_id": modelID, "revision_id": revisionID, "metric_id": varianceID,
		"dim_codes": map[string]string{departmentID: "ENG"}, "value": 999,
	})
	if status != http.StatusForbidden {
		log.Fatalf("ASSERT FAIL: calculated-metric write returned HTTP %d; want 403", status)
	}
	checks++
	fmt.Printf("  PASS  %-34s HTTP %d\n", "write to variance refused", status)

	// ── 7. Result ─────────────────────────────────────────────────────────
	fmt.Printf("\n\033[1;32mMODEL VERIFIED\033[0m  %d/%d checks passed\n", checks, checks)
	fmt.Printf("  application  %s\n", appName)
	fmt.Printf("  app_id       %s\n", appID)
	fmt.Printf("  model_id     %s\n", modelID)
	fmt.Printf("  revision_id  %s\n", revisionID)
	fmt.Printf("  grid_id      %s\n", gridID)
	fmt.Printf("  result       total variance = $55,000 after the update (was $60,000)\n")

	if cleanup {
		admin.call(http.MethodDelete, "/api/admin/applications/"+appID, nil)
		step("CLEANUP set: deleted application %s", appID)
		return
	}
	fmt.Printf("\nOpen the application in the Developer console (Models, Metrics, Grids) or delete it later\n" +
		"through the Admin console. Re-run with CLEANUP=1 to delete it automatically.\n")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
