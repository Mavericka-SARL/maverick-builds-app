package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// setupModelForRunner seeds a minimal real model (region dim + amount metric
// on a grid) so importpkg.ResolveRows can resolve mapped names, then returns
// a store + committer-backed runner in dev mode (loopback fixtures allowed).
func setupRunner(t *testing.T) (*integration.Runner, *integration.Store, string, string, string) {
	t.Helper()
	st, _, modelID, revID := setupStore(t)
	ctx := context.Background()
	pool := st.Pool()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return id
	}
	dimID := q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid,'region',$2::uuid) RETURNING id::text`, modelID, revID)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'A','Region A') RETURNING id::text`, dimID)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'B','Region B') RETURNING id::text`, dimID)
	metricID := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format) VALUES ($1::uuid,$2::uuid,'amount',true,'number') RETURNING id::text`, modelID, revID)
	gridID := q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid,'G',$2::uuid) RETURNING id::text`, modelID, revID)
	if _, err := pool.Exec(ctx, `INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid)`, gridID, dimID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid,$2::uuid,0)`, gridID, metricID); err != nil {
		t.Fatal(err)
	}
	rn := &integration.Runner{
		Store: st, Log: logger.New("test"), AllowInsecure: true,
		Committer: &integration.DBCommitter{Pool: pool, Log: logger.New("test")},
	}
	var custID string
	_ = pool.QueryRow(ctx, `SELECT customer_id::text FROM core.application a JOIN core.model m ON m.application_id=a.id WHERE m.id=$1::uuid`, modelID).Scan(&custID)
	runnerUserID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('runner-user-'||$1,'runner@t.co','R',$2::uuid) RETURNING id::text`, modelID, custID)
	return rn, st, modelID, revID, gridID
}

// runnerUserID is the acting principal claimAndRun enqueues with.
var runnerUserID string

func mkPullDef(t *testing.T, st *integration.Store, modelID, revID, gridID, url string, mutate func(*integration.Config)) *integration.Definition {
	t.Helper()
	cfg := &integration.Config{
		Kind: integration.ConfigKind, Direction: integration.DirectionPull,
		TargetType: integration.TargetGrid, TargetID: gridID,
		ImportMode: integration.ModeIncremental,
		Request:    integration.RequestConfig{Method: "GET", URL: url, BodyMode: integration.BodyNone},
		Auth:       integration.AuthPlacement{Type: "none"},
		Response:   integration.ResponseConfig{Format: integration.FormatJSON, RecordsPath: "$.data.items"},
		Pagination: integration.PaginationConfig{Mode: integration.PageCursor, CursorPath: "$.data.next"},
		Mapping: integration.MappingConfig{Fields: []integration.FieldMap{
			{Source: "$.region", Target: "region"},
			{Source: "$.amount", Target: "amount", Transforms: []integration.Transform{{Kind: integration.TransformToNumber}}},
		}},
	}
	if mutate != nil {
		mutate(cfg)
	}
	def, err := st.CreateDefinition(context.Background(), modelID, revID, "Pull "+url[len(url)-6:], "", nil, "draft", "", cfg, true)
	if err != nil {
		t.Fatalf("create def: %v", err)
	}
	return def
}

func claimAndRun(t *testing.T, rn *integration.Runner, st *integration.Store, defID, trigger string, dry bool) *integration.Run {
	t.Helper()
	ctx := context.Background()
	runID, err := st.Enqueue(ctx, defID, trigger, runnerUserID, dry, nil)
	if err != nil || runID == "" {
		t.Fatalf("enqueue: %v", err)
	}
	run, err := st.Claim(ctx, "test-worker", time.Minute)
	if err != nil || run == nil {
		t.Fatalf("claim: %v", err)
	}
	rn.Execute(ctx, run)
	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return got
}

// TestRunner_PaginatedPullCommitsFacts is the connector's definitive test:
// a two-page cursor-paginated JSON API lands as real fact_input rows through
// the SAME ResolveRows→CommitImport path file uploads use.
func TestRunner_PaginatedPullCommitsFacts(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	rn, st, modelID, revID, gridID := setupRunner(t)

	var pages atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			fmt.Fprint(w, `{"data":{"items":[{"region":"A","amount":10},{"region":"B","amount":"20"}],"next":"c2"}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"items":[{"region":"A","amount":5}],"next":null}}`)
	}))
	defer fixture.Close()

	def := mkPullDef(t, st, modelID, revID, gridID, fixture.URL+"/v1/items", func(c *integration.Config) {
		c.Request.Query = []integration.KV{{Key: "cursor", Value: "{{page.cursor}}", Enabled: true}}
	})
	run := claimAndRun(t, rn, st, def.ID, "manual", false)

	if run.Status != "success" {
		t.Fatalf("run: %+v", run)
	}
	if run.Pages != 2 || run.RecordsRead != 3 || run.RecordsWritten != 3 {
		t.Fatalf("counters: pages=%d read=%d written=%d", run.Pages, run.RecordsRead, run.RecordsWritten)
	}
	// The facts are REALLY there, keyed by the region dimension.
	var n int
	var sum float64
	if err := st.Pool().QueryRow(context.Background(), `
		SELECT COUNT(*), COALESCE(SUM(value),0) FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid
	`, modelID, revID).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	if n != 3 || sum != 35 {
		t.Fatalf("facts: n=%d sum=%v (want 3 rows summing 35)", n, sum)
	}
	// Attempt log carries sanitized URLs (no query string).
	var att int
	var sanitized string
	_ = st.Pool().QueryRow(context.Background(), `
		SELECT COUNT(*), MIN(url_sanitized) FROM model.integration_attempt WHERE run_id=$1::uuid
	`, run.ID).Scan(&att, &sanitized)
	if att != 2 || strings.Contains(sanitized, "cursor=") {
		t.Fatalf("attempts: %d %q", att, sanitized)
	}
}

// TestRunner_ErrorTaxonomy: 401→auth, 429→rate_limit, timeout→timeout,
// invalid JSON→invalid_data, blocked redirect→blocked_host, dry-run writes
// nothing.
func TestRunner_ErrorTaxonomy(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	rn, st, modelID, revID, gridID := setupRunner(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	mux.HandleFunc("/rate", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) { time.Sleep(3 * time.Second) })
	mux.HandleFunc("/badjson", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>nope") })
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest/", http.StatusFound)
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"items":[{"region":"A","amount":1}],"next":null}}`)
	})
	fixture := httptest.NewServer(mux)
	defer fixture.Close()

	cases := []struct {
		path, wantCode string
		mutate         func(*integration.Config)
	}{
		{"/auth", integration.ErrCodeAuth, nil},
		{"/rate", integration.ErrCodeRateLimit, nil},
		{"/slow", integration.ErrCodeTimeout, func(c *integration.Config) { c.Request.TimeoutSeconds = 1 }},
		{"/badjson", integration.ErrCodeInvalidData, nil},
		{"/meta", integration.ErrCodeBlockedHost, nil},
	}
	for _, tc := range cases {
		def := mkPullDef(t, st, modelID, revID, gridID, fixture.URL+tc.path, tc.mutate)
		run := claimAndRun(t, rn, st, def.ID, "manual", false)
		if run.Status != "failed" || run.ErrorCode != tc.wantCode {
			t.Errorf("%s: status=%s code=%s (want failed/%s) msg=%s", tc.path, run.Status, run.ErrorCode, tc.wantCode, run.Message)
		}
	}

	// Dry run: counters but NO writes.
	var before int
	_ = st.Pool().QueryRow(context.Background(), `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, modelID).Scan(&before)
	def := mkPullDef(t, st, modelID, revID, gridID, fixture.URL+"/ok", nil)
	run := claimAndRun(t, rn, st, def.ID, "dry_run", true)
	if run.Status != "success" || run.RecordsWritten != 1 {
		t.Fatalf("dry run: %+v", run)
	}
	var after int
	_ = st.Pool().QueryRow(context.Background(), `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, modelID).Scan(&after)
	if after != before {
		t.Fatalf("dry run wrote rows: %d -> %d", before, after)
	}
}

// TestRunner_TestTriggerPreviewAndMarkTested: a test run captures the
// sanitized preview, stops after one page, writes nothing, and marks the
// config hash tested (unlocking activation).
func TestRunner_TestTriggerPreviewAndMarkTested(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	rn, st, modelID, revID, gridID := setupRunner(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Internal-Auth", "should-not-leak")
		fmt.Fprint(w, `{"data":{"items":[{"region":"A","amount":7}],"next":"more"}}`)
	}))
	defer fixture.Close()

	def := mkPullDef(t, st, modelID, revID, gridID, fixture.URL+"/t", nil)
	run := claimAndRun(t, rn, st, def.ID, "test", false)
	if run.Status != "success" || run.Pages != 1 {
		t.Fatalf("test run: %+v", run)
	}
	var meta map[string]string
	_ = json.Unmarshal(run.Meta, &meta)
	if meta["preview_status"] != "200" || !strings.Contains(meta["preview_body"], `"region":"A"`) {
		t.Fatalf("preview: %v", meta)
	}
	if strings.Contains(meta["preview_headers"], "should-not-leak") {
		t.Fatal("unsafe response header leaked into preview")
	}
	var n int
	_ = st.Pool().QueryRow(context.Background(), `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, modelID).Scan(&n)
	if n != 0 {
		t.Fatal("test run wrote facts")
	}
	got, _ := st.GetDefinition(context.Background(), modelID, def.ID)
	if !got.Tested() {
		t.Fatal("test run did not mark the config tested")
	}
}

// TestRunner_PushPerRecordAndBatch: grid facts flow OUT as one request per
// record and as one batched JSON body; bearer auth is applied; row templates
// resolve.
func TestRunner_PushPerRecordAndBatch(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	rn, st, modelID, revID, gridID := setupRunner(t)
	ctx := context.Background()

	// Source facts: A=10, B=20 on metric amount.
	var metricID string
	_ = st.Pool().QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND name='amount'`, modelID).Scan(&metricID)
	var dimID string
	_ = st.Pool().QueryRow(ctx, `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='region'`, modelID).Scan(&dimID)
	for code, v := range map[string]float64{"A": 10, "B": 20} {
		if _, err := st.Pool().Exec(ctx, `
			INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by)
			VALUES ($1::uuid,$2::uuid,$3::uuid, jsonb_build_object($4::text,$5::text), $6, $7::uuid)
		`, modelID, revID, metricID, dimID, code, v, runnerUserID); err != nil {
			t.Fatal(err)
		}
	}
	// Bearer connection.
	appID := ""
	_ = st.Pool().QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
	conn, err := st.CreateConnection(ctx, appID, "push-target", "bearer", nil, []byte(`{"token":"pushtok"}`), "")
	if err != nil {
		t.Fatal(err)
	}

	type got struct {
		auth, body string
	}
	var mu = make(chan got, 16)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := new(strings.Builder)
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Body.Read(buf)
			b.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		mu <- got{auth: r.Header.Get("Authorization"), body: b.String()}
		w.WriteHeader(200)
	}))
	defer fixture.Close()

	mkPush := func(batch bool) *integration.Definition {
		cfg := &integration.Config{
			Kind: integration.ConfigKind, Direction: integration.DirectionPush,
			TargetType: integration.TargetGrid, TargetID: gridID,
			Request: integration.RequestConfig{
				Method: "POST", URL: fixture.URL + "/ingest", BodyMode: integration.BodyJSON,
				BodyJSON: `{"region":"{{row.region}}","amount":{{row.amount}}}`,
			},
			Auth: integration.AuthPlacement{Type: "bearer"},
			Mapping: integration.MappingConfig{
				Batch: batch, BatchProperty: "records",
				Fields: []integration.FieldMap{
					{Source: "region", Target: "region"},
					{Source: "amount", Target: "amount"},
				},
			},
		}
		def, cerr := st.CreateDefinition(ctx, modelID, revID, fmt.Sprintf("Push batch=%v", batch), "", nil, "draft", conn.ID, cfg, true)
		if cerr != nil {
			t.Fatalf("create push def: %v", cerr)
		}
		return def
	}

	// Per-record: two requests, bearer attached, row templates rendered.
	run := claimAndRun(t, rn, st, mkPush(false).ID, "manual", false)
	if run.Status != "success" || run.RecordsWritten != 2 || run.Requests != 2 {
		t.Fatalf("per-record push: %+v", run)
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		g := <-mu
		if g.auth != "Bearer pushtok" {
			t.Fatalf("auth header: %q", g.auth)
		}
		seen[g.body] = true
	}
	if !seen[`{"region":"A","amount":10}`] || !seen[`{"region":"B","amount":20}`] {
		t.Fatalf("bodies: %v", seen)
	}

	// Batch: ONE request with both records in the wrapper property.
	run = claimAndRun(t, rn, st, mkPush(true).ID, "manual", false)
	if run.Status != "success" || run.RecordsWritten != 2 || run.Requests != 1 {
		t.Fatalf("batch push: %+v", run)
	}
	g := <-mu
	if !strings.HasPrefix(g.body, `{"records":[`) || !strings.Contains(g.body, `"region":"A"`) || !strings.Contains(g.body, `"region":"B"`) {
		t.Fatalf("batch body: %q", g.body)
	}
}

// TestRunner_PersistsWallClockDuration pins run-level duration accounting.
// Regression: execute()'s deferred duration stamp used to land on a dead
// local copy (unnamed result), so every live run persisted duration_ms=0 —
// found on the very first production walkthrough. The stepping clock makes
// the assertion deterministic regardless of how fast the fixture responds.
func TestRunner_PersistsWallClockDuration(t *testing.T) {
	rn, st, modelID, revID, gridID := setupRunner(t)

	base := time.Now()
	var steps atomic.Int64
	rn.Now = func() time.Time {
		return base.Add(time.Duration(steps.Add(1)) * 25 * time.Millisecond)
	}

	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"items":[{"region":"A","amount":1}],"next":null}}`)
	}))
	defer fixture.Close()

	def := mkPullDef(t, st, modelID, revID, gridID, fixture.URL+"/items", func(c *integration.Config) {
		c.Pagination = integration.PaginationConfig{Mode: integration.PageNone}
	})
	run := claimAndRun(t, rn, st, def.ID, "manual", false)
	if run.Status != "success" {
		t.Fatalf("run: %+v", run)
	}
	if run.DurationMS < 25 {
		t.Fatalf("duration_ms=%d — the deferred stamp did not reach the persisted run", run.DurationMS)
	}
}
