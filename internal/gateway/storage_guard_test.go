package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mavericks-engine/mavericks/internal/plan"
)

// A Playground workspace is refused the write that would take it past its
// storage limit, and told so: the whole file, not the part that fits, and
// on any path (a member import over HTTP, facts written straight into the
// database). Deleting a model gives the disk back in the background, and
// moving the workspace to an edition without a limit lifts the guard at
// once.
func TestStorageLimitRefusesTheWriteThatWouldCrossIt(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	id, workspace, tctx := f.tenant(t, "Playground Co")
	pool, err := f.router.Pool(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(tctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	app := seedApp(t, tctx, pool, id, workspace)
	kept := seedModel(t, tctx, pool, app, "Kept", 3000)
	deleted := seedModel(t, tctx, pool, app, "To delete", 60000)
	analyze(t, tctx, pool)
	f.seed(t, tctx, "pg-dev", "dev@playground.test", "Dana", "developer", workspace)
	dev := call{persona: "pg-dev", tenant: id, app: app}

	// The Community Edition's limit, a little above what the workspace
	// holds; the workspace moves onto it and the guard goes on at once.
	base, err := plan.StorageBytes(tctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	limitMB := int(base>>20) + 2
	setCommunityLimit(t, f, limitMB)
	if code, body := do(t, f.srv, f.pa, http.MethodPatch, "/api/admin/tenants/"+id, map[string]string{"plan": "community"}); code != http.StatusOK {
		t.Fatalf("move to community: %d %s", code, body)
	}
	if n := q(`SELECT count(*)::text FROM pg_trigger WHERE tgname = 'storage_guard' AND tgparentid = 0`); n != "4" {
		t.Fatalf("storage guards after the move = %s, want 4", n)
	}

	// A small member file goes through.
	rev := q(`SELECT id::text FROM model.revision WHERE model_id = $1::uuid`, kept)
	code, body := do(t, f.srv, dev, http.MethodPost, "/api/developer/dimensions", map[string]any{"name": "region", "revision_id": rev})
	var dim struct {
		ID string `json:"id"`
	}
	if code != http.StatusOK && code != http.StatusCreated || json.Unmarshal(body, &dim) != nil || dim.ID == "" {
		t.Fatalf("create dimension: %d %s", code, body)
	}
	if code, body := do(t, f.srv, dev, http.MethodPost, "/api/import/dimension-members",
		map[string]any{"dimension_id": dim.ID, "csv": "code,label\nN,North\nS,South\n"}); code != http.StatusOK {
		t.Fatalf("small member file: %d %s", code, body)
	}

	// A file that would cross the limit is refused whole, with the reason.
	var csv strings.Builder
	csv.WriteString("code,label\n")
	for i := range 40000 {
		fmt.Fprintf(&csv, "M%05d,Member number %d with a long enough label to weigh something\n", i, i)
	}
	code, body = do(t, f.srv, dev, http.MethodPost, "/api/import/dimension-members", map[string]any{"dimension_id": dim.ID, "csv": csv.String()})
	var refusal struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(body, &refusal)
	t.Logf("refused: %d %s", code, refusal.Error)
	if code != http.StatusPaymentRequired || refusal.Code != "plan_limit" ||
		!strings.HasPrefix(refusal.Error, "Not saved: this would take the workspace to about ") ||
		!strings.Contains(refusal.Error, fmt.Sprintf("The Community Edition allows %d MB of storage.", limitMB)) {
		t.Fatalf("member file over the limit: %d %s", code, body)
	}
	if n := q(`SELECT count(*)::text FROM model.dimension_member WHERE dimension_id = $1::uuid`, dim.ID); n != "2" {
		t.Fatalf("members after the refused file = %s, want the 2 from before", n)
	}

	// Facts written straight into the database meet the same guard.
	_, err = pool.Exec(tctx, `INSERT INTO runtime.fact_input (model_id, revision_id, dim_members, metric_id, value)
		SELECT model_id, revision_id, dim_members, metric_id, value FROM runtime.fact_input`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != plan.StorageGuardCode {
		t.Fatalf("doubling the facts: %v, want the storage guard", err)
	}

	// Deleting a model: the figure falls at once, the disk in the
	// background.
	files := func() int64 {
		var b int64
		if err := pool.QueryRow(tctx, `SELECT pg_database_size(current_database())`).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	before := files()
	if code, body := do(t, f.srv, call{persona: "held-pa", tenant: id}, http.MethodDelete, "/api/admin/models/"+deleted, nil); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	// The compaction rewrites table after table in the background: wait
	// while it still gives space back, and stop when it has stalled for a
	// minute. A busy machine slows it — on 2026-10-18 it was still going
	// at a fixed 60 s deadline.
	last, stalled, limit := before, time.Now().Add(time.Minute), time.Now().Add(5*time.Minute)
	for n := files(); n > before/2 && time.Now().Before(stalled) && time.Now().Before(limit); n = files() {
		if n < last {
			last, stalled = n, time.Now().Add(time.Minute)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("database files %d MB before the delete, %d MB after", before>>20, files()>>20)
	if files() > before/2 {
		t.Fatalf("database files %d after deleting most of the data, were %d: the disk was not given back", files(), before)
	}

	// An edition without a limit takes the guard off at once.
	if code, body := do(t, f.srv, f.pa, http.MethodPatch, "/api/admin/tenants/"+id, map[string]string{"plan": "enterprise"}); code != http.StatusOK {
		t.Fatalf("move to enterprise: %d %s", code, body)
	}
	if n := q(`SELECT count(*)::text FROM pg_trigger WHERE tgname = 'storage_guard'`); n != "0" {
		t.Fatalf("storage guards on enterprise = %s, want none", n)
	}
	if code, body := do(t, f.srv, dev, http.MethodPost, "/api/import/dimension-members", map[string]any{"dimension_id": dim.ID, "csv": csv.String()}); code != http.StatusOK {
		t.Fatalf("the same file on enterprise: %d %s", code, body)
	}
}

// setCommunityLimit sets the Community Edition's storage limit as the
// platform admin does.
func setCommunityLimit(t *testing.T, f *dedicatedFixture, mb int) {
	t.Helper()
	code, body := do(t, f.srv, f.pa, http.MethodGet, "/api/admin/plans", nil)
	var plans []plan.Plan
	if code != http.StatusOK || json.Unmarshal(body, &plans) != nil {
		t.Fatalf("plans: %d %s", code, body)
	}
	for _, p := range plans {
		if p.Key == "community" {
			p.Limits.MaxStorageMB = mb
			if code, body := do(t, f.srv, f.pa, http.MethodPut, "/api/admin/plans/community", p); code != http.StatusOK {
				t.Fatalf("set community limit: %d %s", code, body)
			}
		}
	}
	f.plans.InvalidateAll()
}
