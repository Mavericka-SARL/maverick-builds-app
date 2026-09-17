// cmd/seed-regional-planning — "Regional Expense Planning" reference demo.
//
// Unlike every other cmd/seed-* program in this repo (all of which write
// directly to Postgres via pgxpool), this one is built ENTIRELY over the
// gateway's HTTP API — the same endpoints a human developer/tenant_admin/
// business_admin would call by clicking through the consoles. That's a
// deliberate, hard project rule: any capability a generated app needs must
// be reachable by a real user in their actual role through the running
// app, not hardcoded via direct SQL a real user could never reproduce.
//
// It first deletes every pre-existing demo application, then builds one
// new, richer demo covering: multi-user dimension-member access rules
// (write/read/hidden), multilevel dimensions (both a cross-dimension
// parent/child pair and a same-dimension hierarchy), metrics with complex
// formulas and metrics that reference other metrics living in a different
// grid with different dimensions, a form integrated with a grid, a
// multi-step workflow with both an automatic (form_submit) and a manual
// trigger, and a laid-out business dashboard.
//
// Requires the gateway already running in dev mode (bash dev.sh) — this
// program is an HTTP client, not a database client.
//
// Usage:
//
//	go run ./cmd/seed-regional-planning
//	GATEWAY_URL=http://localhost:8080 go run ./cmd/seed-regional-planning
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

const revisionName = "Working"

// ── tiny HTTP client, driven exactly like a browser session ────────────────

type api struct {
	base    string
	client  *http.Client
	persona string
	appID   string
	// revisionID/modelID are not sent as headers — they're just carried on
	// the client so every phase's create calls (which each accept
	// revision_id/model_id in their own body) can read them without
	// threading them through every function signature separately.
	revisionID string
	modelID    string
}

func newAPI(base string) *api {
	return &api{base: base, client: &http.Client{Timeout: 30 * time.Second}}
}

// as returns a copy of the client acting as a different persona — mirrors
// switching personas in the dev-persona switcher in the running app.
func (a *api) as(persona string) *api {
	n := *a
	n.persona = persona
	return &n
}

// withRevision returns a copy carrying a working revision ID.
func (a *api) withRevision(revisionID string) *api {
	n := *a
	n.revisionID = revisionID
	return &n
}

// withModel returns a copy carrying a model ID.
func (a *api) withModel(modelID string) *api {
	n := *a
	n.modelID = modelID
	return &n
}

// withApp returns a copy scoped to a specific application — mirrors
// selecting an application in the app switcher.
func (a *api) withApp(appID string) *api {
	n := *a
	n.appID = appID
	return &n
}

// call issues a request and returns the decoded JSON object body. Fatal on
// any transport or non-2xx error — this is a one-shot build script, not a
// long-running service, so fail-fast beats partial/inconsistent state.
func (a *api) call(method, path string, body any) map[string]any {
	raw := a.callRaw(method, path, body)
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatalf("%s %s: decode response: %v (body=%s)", method, path, err, raw)
	}
	return out
}

// callList is like call but for endpoints returning a JSON array.
func (a *api) callList(method, path string, body any) []map[string]any {
	raw := a.callRaw(method, path, body)
	if len(raw) == 0 {
		return nil
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatalf("%s %s: decode response as list: %v (body=%s)", method, path, err, raw)
	}
	return out
}

func (a *api) callRaw(method, path string, body any) []byte {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			log.Fatalf("marshal body for %s %s: %v", method, path, err)
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, a.base+path, buf)
	if err != nil {
		log.Fatalf("build request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.persona != "" {
		req.Header.Set("X-Dev-User", a.persona)
	}
	if a.appID != "" {
		req.Header.Set("X-App-Id", a.appID)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		log.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		log.Fatalf("%s %s (persona=%s app=%s) -> %d: %s", method, path, a.persona, a.appID, resp.StatusCode, raw)
	}
	return raw
}

func id(m map[string]any) string {
	s, _ := m["id"].(string)
	if s == "" {
		log.Fatalf("expected an \"id\" field in response, got %+v", m)
	}
	return s
}

func step(format string, args ...any) {
	fmt.Printf("\033[36m▸\033[0m %s\n", fmt.Sprintf(format, args...))
}

func section(title string) {
	fmt.Printf("\n\033[1m== %s ==\033[0m\n", title)
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	base := os.Getenv("GATEWAY_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	root := newAPI(base)

	section("Phase 0 — tear down existing demos")
	deleteExistingDemos(root.as("platform_admin"))

	section("Phase 1 — bootstrap tenant/workspace/application/model/revision")
	boot := bootstrap(root.as("platform_admin"))

	section("Phase 2 — provision the developer identity")
	dev, boot := provisionDeveloper(root.as("platform_admin"), boot)

	section("Phase 3 — dimensions (cross-dimension + same-dimension hierarchy)")
	dims := buildDimensions(dev)

	section("Phase 4 — metrics (complex formulas + cross-grid references)")
	metrics := buildMetrics(dev, dims)

	section("Phase 5 — grids")
	grids := buildGrids(dev, dims, metrics)

	section("Phase 6 — form integrated with a grid metric")
	frm := buildForm(dev, dims, metrics)

	section("Phase 7 — workflow with an automatic + a manual trigger")
	wf := buildWorkflow(dev, boot, frm)

	section("Phase 8 — users, named role, and per-user dimension access rules")
	users := buildUsersAndAccess(root.as("platform_admin"), boot, dims)

	section("Phase 9 — dashboard")
	dash := buildDashboard(dev, dims, metrics, grids, frm, wf)
	grantFinanceReviewDashboardAccess(root.as("platform_admin"), boot, users.financeReviewRoleID, dash.dashboardID)

	section("Phase 10 — seed input facts")
	seedFacts(dev, dims, metrics)

	section("Done")
	summary(boot, dims, metrics, grids, frm, wf, users, dash)
}
