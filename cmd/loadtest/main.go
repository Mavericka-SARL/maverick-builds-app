// cmd/loadtest drives a running deployment the way a room full of planners
// does: N virtual users, each writing cells into one model and reading the
// grid back, for a fixed duration. It reports latency percentiles, error
// counts and throughput per operation, so a change to the write path — cell
// writes recalculate dependants synchronously, in the request — is measured
// rather than guessed at.
//
// It talks HTTP only, like the console, and never touches the database, so
// it runs against the dev stack, staging or production alike; what differs
// is how it authenticates.
//
//	# dev stack, persona auth, a fresh trial tenant with the starter model:
//	go run ./cmd/loadtest -base http://localhost:8080 -signup -users 50 -duration 60s
//
//	# staging/production, a real account (password grant on the realm):
//	go run ./cmd/loadtest -base https://staging.example.com \
//	  -keycloak https://auth-staging.example.com -username dev@example.com -password ... \
//	  -users 100 -duration 2m -target-p95 2s
//
//	# before DNS exists: pin hosts and accept the placeholder certificate
//	  -resolve staging.example.com=203.0.113.10 -resolve auth-staging.example.com=203.0.113.10 -insecure
//
// The model under load is discovered from the account's application
// (/api/demo → grids, metrics, dimensions) unless -grid/-metric name it. A
// write is one POST /api/cells on a random input metric of the grid at a
// random leaf intersection; a read is GET /api/grid for the same grid.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mavericks-engine/mavericks/internal/starter"
)

type resolveFlag map[string]string

func (r resolveFlag) String() string { return fmt.Sprint(map[string]string(r)) }
func (r resolveFlag) Set(v string) error {
	host, ip, ok := strings.Cut(v, "=")
	if !ok || host == "" || ip == "" {
		return fmt.Errorf("-resolve wants host=ip, got %q", v)
	}
	r[host] = ip
	return nil
}

type config struct {
	base, token, devUser                string
	keycloak, realm, username, password string
	app, model, revision, grid, metric  string
	users                               int
	duration, ramp, think               time.Duration
	writeRatio                          float64
	targetP95                           time.Duration
	jsonOut                             string
	signup                              bool
	seed                                bool
	insecure                            bool
	resolve                             resolveFlag
}

func main() {
	var c config
	c.resolve = resolveFlag{}
	flag.StringVar(&c.base, "base", envOr("GATEWAY_URL", "http://localhost:8080"), "gateway origin")
	flag.StringVar(&c.token, "token", os.Getenv("MAVERICKS_TOKEN"), "bearer token (or MAVERICKS_TOKEN)")
	flag.StringVar(&c.devUser, "dev-user", "", "X-Dev-User persona (dev stack only)")
	flag.StringVar(&c.keycloak, "keycloak", os.Getenv("KEYCLOAK_URL"), "Keycloak origin for a password grant (with -username/-password)")
	flag.StringVar(&c.realm, "realm", envOr("KEYCLOAK_REALM", "mavericks"), "Keycloak realm")
	flag.StringVar(&c.username, "username", os.Getenv("MAVERICKS_USERNAME"), "account for the password grant")
	flag.StringVar(&c.password, "password", os.Getenv("MAVERICKS_PASSWORD"), "its password")
	flag.StringVar(&c.app, "app", "", "application id (default: the account's, via /api/demo)")
	flag.StringVar(&c.model, "model", "", "model id (default: from /api/demo)")
	flag.StringVar(&c.revision, "revision", "", "revision id (default: from /api/demo)")
	flag.StringVar(&c.grid, "grid", "", "grid name to drive (default: the first grid of the model)")
	flag.StringVar(&c.metric, "metric", "", "input metric name to write (default: every input metric of the grid, at random)")
	flag.IntVar(&c.users, "users", 20, "concurrent virtual users")
	flag.DurationVar(&c.duration, "duration", 30*time.Second, "how long to run after the ramp")
	flag.DurationVar(&c.ramp, "ramp", 5*time.Second, "time over which users start")
	flag.DurationVar(&c.think, "think", 0, "pause between a user's operations")
	flag.Float64Var(&c.writeRatio, "write-ratio", 0.5, "share of operations that are cell writes (the rest read the grid)")
	flag.DurationVar(&c.targetP95, "target-p95", 0, "exit 1 when any operation's p95 exceeds this")
	flag.StringVar(&c.jsonOut, "json", "", "also write the report as JSON to this file")
	flag.BoolVar(&c.signup, "signup", false, "dev stack: create a fresh trial tenant through /api/signup and load its starter model")
	flag.BoolVar(&c.seed, "seed", false, "as a tenant admin: create an application in the account's tenant and import the starter model into it, then load that")
	flag.BoolVar(&c.insecure, "insecure", false, "accept any TLS certificate (before DNS/certificates exist)")
	flag.Var(c.resolve, "resolve", "host=ip, pin a hostname to an address (repeatable)")
	flag.Parse()

	client := newClient(c)
	ctx := context.Background()

	if c.token == "" && c.devUser == "" && c.keycloak != "" && c.username != "" {
		tok, err := passwordGrant(ctx, client, c)
		if err != nil {
			fatalf("token: %v", err)
		}
		c.token = tok
		step("token obtained for %s", c.username)
	}
	api := &api{c: c, client: client}

	if c.signup {
		if c.token != "" {
			fatalf("-signup is for the dev stack (persona auth), not for a real account")
		}
		email := fmt.Sprintf("load-%d@loadtest.example", time.Now().UnixNano())
		var out struct {
			DevPersona string `json:"dev_persona"`
			TenantID   string `json:"tenant_id"`
			AppID      string `json:"application_id"`
			ModelID    string `json:"model_id"`
		}
		if err := api.call(ctx, http.MethodPost, "/api/signup", map[string]any{"company": "Load Test " + time.Now().Format("15:04:05"), "first_name": "Load", "last_name": "Tester", "email": email}, &out); err != nil {
			fatalf("signup: %v (is SIGNUP_ENABLED=true on the gateway?)", err)
		}
		if out.DevPersona == "" {
			fatalf("signup answered without dev_persona: the gateway is not in dev mode")
		}
		api.c.devUser, api.c.app, api.c.model = out.DevPersona, out.AppID, out.ModelID
		step("signed up tenant %s as %s (app %s, model %s)", out.TenantID, out.DevPersona, out.AppID, out.ModelID)
	}
	if api.c.token == "" && api.c.devUser == "" {
		fatalf("no credentials: pass -token, -dev-user, or -keycloak with -username/-password")
	}
	if c.seed {
		if err := seedStarter(ctx, api); err != nil {
			fatalf("seed: %v", err)
		}
	}

	target, err := discover(ctx, api)
	if err != nil {
		fatalf("discover: %v", err)
	}
	step("model %s revision %s — grid %q, %d input metrics, %d dimensions (%s)",
		target.modelID, target.revisionID, target.gridName, len(target.inputMetrics), len(target.dims), target.dimSummary())

	report := run(ctx, api, target)
	report.print(os.Stdout)
	if c.jsonOut != "" {
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(c.jsonOut, b, 0o644); err != nil {
			fatalf("write %s: %v", c.jsonOut, err)
		}
	}
	if c.targetP95 > 0 {
		for _, op := range report.Operations {
			if time.Duration(op.P95Ms*float64(time.Millisecond)) > c.targetP95 {
				fmt.Fprintf(os.Stderr, "FAIL: %s p95 %.0f ms exceeds target %s\n", op.Name, op.P95Ms, c.targetP95)
				os.Exit(1)
			}
		}
	}
	if report.Errors > 0 {
		os.Exit(1)
	}
}

// ── HTTP ────────────────────────────────────────────────────────────────────

func newClient(c config) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		MaxIdleConns: c.users * 2, MaxIdleConnsPerHost: c.users * 2, IdleConnTimeout: 90 * time.Second,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.insecure}, //nolint:gosec // opt-in, for hosts without DNS yet
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err == nil {
				if ip, ok := c.resolve[host]; ok {
					addr = net.JoinHostPort(ip, port)
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{Transport: tr, Timeout: 120 * time.Second}
}

func passwordGrant(ctx context.Context, client *http.Client, c config) (string, error) {
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {c.username}, "password": {c.password}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.keycloak, "/")+"/realms/"+c.realm+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%d: %s", resp.StatusCode, body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("no access_token in %s", body)
	}
	return out.AccessToken, nil
}

type api struct {
	c      config
	client *http.Client
}

// do performs one request and returns status, body and latency.
func (a *api) do(ctx context.Context, method, path string, body any) (int, []byte, time.Duration, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.c.base+path, rdr)
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.c.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.c.token)
	}
	if a.c.devUser != "" {
		req.Header.Set("X-Dev-User", a.c.devUser)
	}
	if a.c.app != "" {
		req.Header.Set("X-App-Id", a.c.app)
	}
	t0 := time.Now()
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, time.Since(t0), err
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, time.Since(t0), nil
}

func (a *api) call(ctx context.Context, method, path string, body, out any) error {
	status, raw, _, err := a.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("%s %s → %d: %s", method, path, status, truncate(raw))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// seedStarter gives the account something to load on a deployment without
// sign-up: an application in its own tenant holding the starter model
// (internal/starter), imported the way a tenant admin imports a package.
// The account needs the tenant_admin role (to import) and developer (to
// read grids and dimensions during discovery).
func seedStarter(ctx context.Context, a *api) error {
	var me struct {
		CustomerID string `json:"customer_id"`
		Roles      []string
	}
	if err := a.call(ctx, http.MethodGet, "/api/me", nil, &me); err != nil {
		return err
	}
	if me.CustomerID == "" {
		// A tenant admin created before accounts carried customer_id (or a
		// gateway that predates the field): the tenant list is scoped to
		// what the account administers.
		var tenants []struct {
			ID string `json:"id"`
		}
		if err := a.call(ctx, http.MethodGet, "/api/admin/tenants", nil, &tenants); err != nil {
			return err
		}
		if len(tenants) != 1 {
			return fmt.Errorf("the account administers %d tenants; it needs exactly one (create it under Platform › Applications and make this account its tenant_admin + developer)", len(tenants))
		}
		me.CustomerID = tenants[0].ID
	}
	var app struct {
		ID string `json:"id"`
	}
	name := "Load test " + time.Now().Format("2006-01-02 15:04")
	if err := a.call(ctx, http.MethodPost, "/api/admin/applications", map[string]any{"customer_id": me.CustomerID, "name": name, "mode": "planning"}, &app); err != nil {
		return fmt.Errorf("create application: %w", err)
	}
	var imported struct {
		ModelID    string `json:"model_id"`
		RevisionID string `json:"revision_id"`
	}
	if err := a.call(ctx, http.MethodPost, "/api/admin/models/import", map[string]any{"application_id": app.ID, "package": starter.Package()}, &imported); err != nil {
		return fmt.Errorf("import starter model: %w", err)
	}
	a.c.app, a.c.model, a.c.revision = app.ID, imported.ModelID, imported.RevisionID
	step("seeded application %q (%s) with the starter model %s", name, app.ID, imported.ModelID)
	return nil
}

// ── discovery ───────────────────────────────────────────────────────────────

type dimension struct {
	id, name string
	leaves   []string // member codes with no children
}

type target struct {
	modelID, revisionID, gridID, gridName string
	inputMetrics                          []struct{ id, name string }
	dims                                  []dimension
}

func (t *target) dimSummary() string {
	parts := make([]string, 0, len(t.dims))
	for _, d := range t.dims {
		parts = append(parts, fmt.Sprintf("%s: %d leaves", d.name, len(d.leaves)))
	}
	return strings.Join(parts, ", ")
}

func discover(ctx context.Context, a *api) (*target, error) {
	t := &target{modelID: a.c.model, revisionID: a.c.revision}
	if a.c.app == "" || t.modelID == "" || t.revisionID == "" {
		var demo struct {
			AppID, ModelID, RevisionID string
		}
		var raw map[string]any
		if err := a.call(ctx, http.MethodGet, "/api/demo", nil, &raw); err != nil && (a.c.app == "" || t.modelID == "") {
			return nil, fmt.Errorf("/api/demo: %w", err)
		}
		demo.AppID, _ = raw["app_id"].(string)
		demo.ModelID, _ = raw["model_id"].(string)
		demo.RevisionID, _ = raw["revision_id"].(string)
		if a.c.app == "" {
			a.c.app = demo.AppID
		}
		if t.modelID == "" {
			t.modelID = demo.ModelID
		}
		if t.revisionID == "" {
			t.revisionID = demo.RevisionID
		}
	}
	if t.modelID == "" {
		return nil, fmt.Errorf("no model: the account's application has none, or pass -model")
	}

	var grids []struct {
		ID, Name     string
		MetricIDs    []string `json:"metric_ids"`
		DimensionIDs []string `json:"dimension_ids"`
		RevisionID   string   `json:"revision_id"`
	}
	if err := a.call(ctx, http.MethodGet, "/api/developer/grids", nil, &grids); err != nil {
		return nil, fmt.Errorf("grids: %w", err)
	}
	var chosen *struct {
		ID, Name     string
		MetricIDs    []string `json:"metric_ids"`
		DimensionIDs []string `json:"dimension_ids"`
		RevisionID   string   `json:"revision_id"`
	}
	for i := range grids {
		g := &grids[i]
		if g.RevisionID != "" && g.RevisionID != t.revisionID {
			continue
		}
		if a.c.grid == "" || strings.EqualFold(g.Name, a.c.grid) {
			chosen = g
			break
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("no grid %q in revision %s (found %d grids)", a.c.grid, t.revisionID, len(grids))
	}
	t.gridID, t.gridName = chosen.ID, chosen.Name
	if t.revisionID == "" {
		t.revisionID = chosen.RevisionID
	}

	var metrics []struct {
		ID, Name string
		IsInput  bool `json:"is_input"`
	}
	if err := a.call(ctx, http.MethodGet, "/api/metrics", nil, &metrics); err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	inGrid := map[string]bool{}
	for _, id := range chosen.MetricIDs {
		inGrid[id] = true
	}
	for _, m := range metrics {
		if m.IsInput && inGrid[m.ID] && (a.c.metric == "" || strings.EqualFold(m.Name, a.c.metric)) {
			t.inputMetrics = append(t.inputMetrics, struct{ id, name string }{m.ID, m.Name})
		}
	}
	if len(t.inputMetrics) == 0 {
		return nil, fmt.Errorf("the grid %q has no input metric to write (%q)", chosen.Name, a.c.metric)
	}

	var dims []struct {
		ID, Name string
		Members  []struct {
			ID, Code       string
			ParentMemberID *string `json:"parent_member_id"`
		} `json:"members"`
	}
	if err := a.call(ctx, http.MethodGet, "/api/developer/dimensions", nil, &dims); err != nil {
		return nil, fmt.Errorf("dimensions: %w", err)
	}
	wanted := map[string]bool{}
	for _, id := range chosen.DimensionIDs {
		wanted[id] = true
	}
	for _, d := range dims {
		if !wanted[d.ID] {
			continue
		}
		members := d.Members
		if len(members) == 0 {
			if err := a.call(ctx, http.MethodGet, "/api/developer/dimensions/"+d.ID+"/members", nil, &members); err != nil {
				return nil, fmt.Errorf("members of %s: %w", d.Name, err)
			}
		}
		parents := map[string]bool{}
		for _, m := range members {
			if m.ParentMemberID != nil {
				parents[*m.ParentMemberID] = true
			}
		}
		dim := dimension{id: d.ID, name: d.Name}
		for _, m := range members {
			if !parents[m.ID] {
				dim.leaves = append(dim.leaves, m.Code)
			}
		}
		if len(dim.leaves) == 0 {
			return nil, fmt.Errorf("dimension %s has no leaf members", d.Name)
		}
		t.dims = append(t.dims, dim)
	}
	if len(t.dims) != len(chosen.DimensionIDs) {
		return nil, fmt.Errorf("grid %q names %d dimensions, %d found", chosen.Name, len(chosen.DimensionIDs), len(t.dims))
	}
	return t, nil
}

// ── the run ─────────────────────────────────────────────────────────────────

type sample struct {
	op      string
	latency time.Duration
	status  int
	err     error
}

type opStats struct {
	Name     string   `json:"name"`
	Requests int      `json:"requests"`
	Errors   int      `json:"errors"`
	RPS      float64  `json:"rps"`
	P50Ms    float64  `json:"p50_ms"`
	P90Ms    float64  `json:"p90_ms"`
	P95Ms    float64  `json:"p95_ms"`
	P99Ms    float64  `json:"p99_ms"`
	MaxMs    float64  `json:"max_ms"`
	MeanMs   float64  `json:"mean_ms"`
	Examples []string `json:"error_examples,omitempty"`
}

type reportT struct {
	Base       string    `json:"base"`
	Users      int       `json:"users"`
	Duration   string    `json:"duration"`
	WriteRatio float64   `json:"write_ratio"`
	Model      string    `json:"model_id"`
	Grid       string    `json:"grid"`
	Started    time.Time `json:"started_at"`
	Requests   int       `json:"requests"`
	Errors     int       `json:"errors"`
	Operations []opStats `json:"operations"`
}

func run(ctx context.Context, a *api, t *target) *reportT {
	var (
		mu             sync.Mutex
		samples        []sample
		inFlight, done atomic.Int64
	)
	record := func(s sample) {
		mu.Lock()
		samples = append(samples, s)
		mu.Unlock()
	}
	start := time.Now()
	deadline := start.Add(a.c.ramp + a.c.duration)
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var wg sync.WaitGroup
	for u := 0; u < a.c.users; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			// Users start spread over the ramp, so the first second is not
			// N simultaneous connections against a cold server.
			if a.c.ramp > 0 {
				select {
				case <-time.After(time.Duration(float64(a.c.ramp) * float64(u) / float64(a.c.users))):
				case <-runCtx.Done():
					return
				}
			}
			rng := rand.New(rand.NewSource(int64(u) + start.UnixNano())) //nolint:gosec // load shape, not security
			for runCtx.Err() == nil {
				inFlight.Add(1)
				if rng.Float64() < a.c.writeRatio {
					m := t.inputMetrics[rng.Intn(len(t.inputMetrics))]
					codes := map[string]string{}
					for _, d := range t.dims {
						codes[d.id] = d.leaves[rng.Intn(len(d.leaves))]
					}
					status, raw, lat, err := a.do(runCtx, http.MethodPost, "/api/cells", map[string]any{
						"model_id": t.modelID, "revision_id": t.revisionID, "metric_id": m.id, "dim_codes": codes,
						"value": float64(rng.Intn(100000)),
					})
					record(sample{op: "POST /api/cells", latency: lat, status: status, err: errOf(status, raw, err, runCtx)})
				} else {
					status, raw, lat, err := a.do(runCtx, http.MethodGet, "/api/grid?grid_def_id="+url.QueryEscape(t.gridID)+"&revision_id="+url.QueryEscape(t.revisionID), nil)
					record(sample{op: "GET /api/grid", latency: lat, status: status, err: errOf(status, raw, err, runCtx)})
				}
				inFlight.Add(-1)
				done.Add(1)
				if a.c.think > 0 {
					select {
					case <-time.After(a.c.think):
					case <-runCtx.Done():
					}
				}
			}
		}(u)
	}
	// Progress once a second, so a stalled server is visible before the end.
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				step("%6.0fs  %7d done  %3d in flight", time.Since(start).Seconds(), done.Load(), inFlight.Load())
			}
		}
	}()
	wg.Wait()
	ticker.Stop()
	elapsed := time.Since(start)

	byOp := map[string][]sample{}
	for _, s := range samples {
		byOp[s.op] = append(byOp[s.op], s)
	}
	r := &reportT{Base: a.c.base, Users: a.c.users, Duration: elapsed.Round(time.Second).String(), WriteRatio: a.c.writeRatio,
		Model: t.modelID, Grid: t.gridName, Started: start, Requests: len(samples)}
	names := make([]string, 0, len(byOp))
	for n := range byOp {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ss := byOp[n]
		lat := make([]float64, 0, len(ss))
		st := opStats{Name: n, Requests: len(ss)}
		var sum float64
		for _, s := range ss {
			if s.err != nil {
				st.Errors++
				if len(st.Examples) < 3 {
					st.Examples = append(st.Examples, s.err.Error())
				}
				continue
			}
			ms := float64(s.latency) / float64(time.Millisecond)
			lat = append(lat, ms)
			sum += ms
		}
		sort.Float64s(lat)
		if len(lat) > 0 {
			st.P50Ms, st.P90Ms, st.P95Ms, st.P99Ms = pct(lat, 50), pct(lat, 90), pct(lat, 95), pct(lat, 99)
			st.MaxMs, st.MeanMs = lat[len(lat)-1], sum/float64(len(lat))
		}
		st.RPS = float64(len(ss)) / elapsed.Seconds()
		r.Errors += st.Errors
		r.Operations = append(r.Operations, st)
	}
	return r
}

func errOf(status int, raw []byte, err error, ctx context.Context) error {
	if err != nil {
		if ctx.Err() != nil {
			return nil // cut off by the deadline, not a failure
		}
		return err
	}
	if status >= 400 {
		return fmt.Errorf("%d: %s", status, truncate(raw))
	}
	return nil
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p / 100)
	return sorted[i]
}

func (r *reportT) print(w io.Writer) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("\n%s — %d users for %s (write ratio %.0f%%), model %s, grid %q\n", r.Base, r.Users, r.Duration, r.WriteRatio*100, r.Model, r.Grid)
	p("%-18s %8s %6s %8s %8s %8s %8s %8s %8s\n", "operation", "requests", "errors", "rps", "p50 ms", "p90 ms", "p95 ms", "p99 ms", "max ms")
	for _, op := range r.Operations {
		p("%-18s %8d %6d %8.1f %8.0f %8.0f %8.0f %8.0f %8.0f\n", op.Name, op.Requests, op.Errors, op.RPS, op.P50Ms, op.P90Ms, op.P95Ms, op.P99Ms, op.MaxMs)
		for _, e := range op.Examples {
			p("    error: %s\n", e)
		}
	}
	p("total %d requests, %d errors\n", r.Requests, r.Errors)
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func step(format string, args ...any) { fmt.Fprintf(os.Stderr, "  "+format+"\n", args...) }
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "loadtest: "+format+"\n", args...)
	os.Exit(2)
}
