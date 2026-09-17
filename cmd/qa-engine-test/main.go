// cmd/qa-engine-test — engine QA smoke test, driven entirely over the same
// HTTP API a developer/business user would use (see cmd/seed-regional-planning
// for why that's a hard project rule, not a style choice).
//
// Two things get exercised:
//
//  1. Formula correctness: every function in internal/formula/functions.go,
//     every operator, cross-metric references, dimension-conditional
//     formulas, agg_rule=average, and all 7 form->metric aggregation modes
//     (sum/average/min/max/count/last/replace) — including the
//     direct-entry-vs-form-posted interaction that a prior session found a
//     real data-loss bug in (see model_transfer.go's export/import fix).
//
//  2. Performance at scale: a 10,000-cell bulk CSV import (100x100
//     dimension), timed; a single-cell "adjustment" writeback timed before
//     and after that bulk data exists (to see whether editing one cell
//     stays cheap once the model is no longer tiny); and the backend
//     calc-result recalculation that both paths trigger.
//
// Requires the gateway already running in dev mode (bash dev.sh).
//
// Usage:
//
//	go run ./cmd/qa-engine-test
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

// ── tiny HTTP client (same pattern as cmd/seed-regional-planning) ──────────

type api struct {
	base       string
	client     *http.Client
	persona    string
	appID      string
	revisionID string
	modelID    string
}

func newAPI(base string) *api {
	return &api{base: base, client: &http.Client{Timeout: 180 * time.Second}}
}

func (a *api) as(persona string) *api              { n := *a; n.persona = persona; return &n }
func (a *api) withApp(appID string) *api           { n := *a; n.appID = appID; return &n }
func (a *api) withRevision(revisionID string) *api { n := *a; n.revisionID = revisionID; return &n }
func (a *api) withModel(modelID string) *api       { n := *a; n.modelID = modelID; return &n }

func (a *api) call(method, path string, body any) map[string]any {
	raw, status := a.callRaw(method, path, body)
	if status >= 300 {
		log.Fatalf("%s %s -> %d: %s", method, path, status, raw)
	}
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatalf("%s %s: decode: %v (body=%s)", method, path, err, raw)
	}
	return out
}

// callStatus is a non-fatal variant for negative tests (expecting 4xx).
func (a *api) callStatus(method, path string, body any) (map[string]any, int) {
	raw, status := a.callRaw(method, path, body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, status
}

// callList is call's counterpart for endpoints returning a JSON array.
func (a *api) callList(method, path string, body any) []map[string]any {
	raw, status := a.callRaw(method, path, body)
	if status >= 300 {
		log.Fatalf("%s %s -> %d: %s", method, path, status, raw)
	}
	if len(raw) == 0 {
		return nil
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatalf("%s %s: decode: %v (body=%s)", method, path, err, raw)
	}
	return out
}

func (a *api) callRaw(method, path string, body any) ([]byte, int) {
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
	return raw, resp.StatusCode
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
func section(title string) { fmt.Printf("\n\033[1m== %s ==\033[0m\n", title) }
func warn(format string, args ...any) {
	fmt.Printf("\033[33m⚠\033[0m %s\n", fmt.Sprintf(format, args...))
}

// ── result tracking ─────────────────────────────────────────────────────────

type result struct {
	name string
	pass bool
	note string
}

var results []result

func record(name string, pass bool, note string) {
	results = append(results, result{name, pass, note})
	mark := "\033[32m✓\033[0m"
	if !pass {
		mark = "\033[31m✗\033[0m"
	}
	if note != "" {
		fmt.Printf("  %s %-32s %s\n", mark, name, note)
	} else {
		fmt.Printf("  %s %s\n", mark, name)
	}
}

// ── main ─────────────────────────────────────────────────────────────────

func main() {
	base := os.Getenv("GATEWAY_URL")
	if base == "" {
		base = "http://localhost:8080"
	}
	root := newAPI(base)

	section("Phase 0 — bootstrap throwaway QA app")
	dev := bootstrap(root)

	section("Phase 1 — formula function/operator coverage")
	seedID := runFormulaCoverage(dev)

	section("Phase 2 — dimension-conditional formulas, cross-metric refs, agg_rule=average")
	runDimensionalTests(dev)

	section("Phase 3 — form -> metric aggregation modes (all 7)")
	runFormAggregationTests(dev)

	section("Phase 4 — performance: 10,000-cell bulk import + adjustments + sync")
	runPerfTests(dev, seedID)

	section("Summary")
	printSummary()
}
