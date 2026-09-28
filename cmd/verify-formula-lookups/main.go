// cmd/verify-formula-lookups — live end-to-end check of the formula
// dimensional references, LOOKUP, conditional aggregation and the newer time
// functions, driven entirely over the running gateway's HTTP API as the real
// roles would drive it (project standing rule: seed and verify scripts never
// touch the database).
//
// It builds a throwaway tenant as platform_admin, then, as a developer:
//
//   - region (World > EMEA > DE, UK; World > AMER > US) with typed properties
//     segment (text), factor (number) and currency (text);
//   - currency (EUR, GBP, USD);
//   - a monthly time dimension with FY/H/Q aggregate periods;
//   - input metrics revenue [region, month], lag_n [region, month] and
//     fx_rate [currency];
//   - calculated metrics covering dim.property, PARENT, LOOKUP (literal leaf,
//     literal parent, dynamic via region.currency), SUMIFS/AVERAGEIFS/
//     COUNTIFS/MINIFS/MAXIFS by property and by code, SUMIF, same-segment
//     SUMIFS, a dynamic LAG offset, YEARVALUE, QUARTERVALUE, HALFYEARTODATE,
//     TIMESUM, START/END, DAYSINMONTH/DAYSINYEAR and bare-dimension IFs at a
//     total (agg_rule formula).
//
// It writes facts through POST /api/cells, waits for the recalculation, and
// asserts exact numbers — computed here, independently, from the facts it
// wrote — in GET /api/grid cells and totals and in chart-data. It then checks
// property rename/delete, the documented 400 codes, a restricted business
// user's withheld cells, and that the business user's hidden member stays
// hidden on the old revision after a new revision is activated. Finally, in a
// fresh revision, it proves the final review's fixes live (regressions.go).
//
// Every check prints one PASS/FAIL line; the exit status is non-zero when any
// check fails.
//
// Requires a gateway in DEV_MODE (X-Dev-User personas) whose database already
// has a platform administrator reachable as the -admin persona (the dev
// database's seeded "platform_admin", or one created by
// scripts/bootstrap-platform-admin.sh).
//
// Usage:
//
//	go run ./cmd/verify-formula-lookups [-base http://localhost:8081] [-keep]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const tenantName = "Formula Lookups Verify Co"

var (
	flagBase    = flag.String("base", "http://localhost:8081", "gateway base URL")
	flagAdmin   = flag.String("admin", "platform_admin", "X-Dev-User persona of an existing platform administrator")
	flagKeep    = flag.Bool("keep", false, "keep the tenant after the run (default: delete it)")
	flagTimeout = flag.Duration("recalc-timeout", 45*time.Second, "how long to wait for a recalculation to settle")
)

// ── HTTP client ─────────────────────────────────────────────────────────────

type api struct {
	base    string
	client  *http.Client
	persona string
	appID   string
}

func (a *api) as(persona string) *api    { n := *a; n.persona = persona; return &n }
func (a *api) withApp(appID string) *api { n := *a; n.appID = appID; return &n }

// fatalError aborts the run from a setup step; main recovers it so the
// summary and teardown still run.
type fatalError struct{ msg string }

func fatalf(format string, args ...any) {
	panic(fatalError{fmt.Sprintf(format, args...)})
}

func (a *api) raw(method, path string, body any) ([]byte, int) {
	var buf io.Reader
	var sent []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			fatalf("marshal body for %s %s: %v", method, path, err)
		}
		sent = b
		buf = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, buf)
	if err != nil {
		fatalf("build request %s %s: %v", method, path, err)
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
		fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	out, _ := io.ReadAll(resp.Body)
	if *flagVerbose {
		fmt.Printf("    %s %s %s -> %d %s\n", method, path, clip(string(sent), 300), resp.StatusCode, clip(string(out), 300))
	}
	return out, resp.StatusCode
}

var flagVerbose = flag.Bool("v", false, "print every request and response (clipped)")

// call is a setup call that must succeed; a failure aborts the run with the
// exact request and response.
func (a *api) call(method, path string, body any) map[string]any {
	raw, status := a.raw(method, path, body)
	if status >= 300 {
		b, _ := json.Marshal(body)
		fatalf("%s %s (as %s) body=%s -> %d: %s", method, path, a.persona, clip(string(b), 600), status, clip(string(raw), 600))
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			fatalf("%s %s: decode: %v (body=%s)", method, path, err, clip(string(raw), 300))
		}
	}
	return out
}

// try is a call whose status the caller checks (negative tests).
func (a *api) try(method, path string, body any) (string, int) {
	raw, status := a.raw(method, path, body)
	return string(raw), status
}

func (a *api) list(method, path string, body any) []map[string]any {
	raw, status := a.raw(method, path, body)
	if status >= 300 {
		fatalf("%s %s (as %s) -> %d: %s", method, path, a.persona, status, clip(string(raw), 600))
	}
	var out []map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &out); err != nil {
			fatalf("%s %s: decode list: %v (body=%s)", method, path, err, clip(string(raw), 300))
		}
	}
	return out
}

func id(m map[string]any) string {
	s, _ := m["id"].(string)
	if s == "" {
		fatalf("expected an \"id\" in response, got %v", m)
	}
	return s
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── results ────────────────────────────────────────────────────────────────

type result struct {
	name string
	pass bool
	note string
}

var results []result

func check(name string, pass bool, format string, args ...any) {
	note := fmt.Sprintf(format, args...)
	results = append(results, result{name, pass, note})
	tag := "PASS"
	if !pass {
		tag = "FAIL"
	}
	if note != "" {
		fmt.Printf("%s %s — %s\n", tag, name, note)
	} else {
		fmt.Printf("%s %s\n", tag, name)
	}
}

func section(title string) { fmt.Printf("\n== %s ==\n", title) }
func step(format string, args ...any) {
	fmt.Printf("  · %s\n", fmt.Sprintf(format, args...))
}

// ── main ───────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()
	root := &api{base: strings.TrimRight(*flagBase, "/"), client: &http.Client{Timeout: 90 * time.Second}}
	started := time.Now()

	var env *environment
	func() {
		defer func() {
			if r := recover(); r != nil {
				fe, ok := r.(fatalError)
				if !ok {
					panic(r)
				}
				check("run completed without an aborting setup error", false, "%s", fe.msg)
			}
		}()
		section("Bootstrap (platform_admin)")
		env = bootstrap(root)

		section("Model (developer)")
		buildModel(env)

		section("Facts and recalculation")
		writeFacts(env)
		checkDeveloperGrid(env)

		section("Chart-data (developer)")
		checkDeveloperCharts(env)

		section("Save-time validation")
		checkBadFormulas(env)

		section("Property declarations")
		checkProperties(env)

		section("Restricted viewer")
		checkRestrictedViewer(env)

		section("Access rules across revisions")
		checkRevisionLineage(env)

		checkRegressions(env)
	}()

	if env != nil && env.tenantID != "" && !*flagKeep {
		if _, status := root.as(*flagAdmin).try("DELETE", "/api/admin/tenants/"+env.tenantID, nil); status >= 300 {
			fmt.Printf("\nnote: teardown DELETE tenant %s -> %d\n", env.tenantID, status)
		}
	}

	section("Summary")
	failed := 0
	for _, r := range results {
		if !r.pass {
			failed++
		}
	}
	if failed > 0 {
		fmt.Println("Failed checks:")
		names := make([]string, 0, failed)
		for _, r := range results {
			if !r.pass {
				names = append(names, "  - "+r.name)
			}
		}
		sort.Strings(names)
		fmt.Println(strings.Join(names, "\n"))
	}
	fmt.Printf("%d checks, %d passed, %d failed (%s)\n", len(results), len(results)-failed, failed, time.Since(started).Round(time.Millisecond))
	if failed > 0 || len(results) == 0 {
		os.Exit(1)
	}
}

func idFromRaw(raw string) string {
	out := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		fatalf("decode %s: %v", clip(raw, 200), err)
	}
	return id(out)
}
