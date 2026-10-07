package gateway

// Member order is one thing everywhere: grids and charts honour
// dimension_member.sort_order (period first on a time dimension), so every
// create path appends and every member endpoint sorts the same way. And a
// revision_id in a request BODY that is not a revision of the model — a
// malformed one included — is a 404, as the same id in ?revision_id= is.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// memberCodes returns dimension dimID's member codes from a GET /api/dimensions
// response, in the order it sends them.
func memberCodes(t *testing.T, raw []byte, dimID string) []string {
	t.Helper()
	var dims []struct {
		ID      string `json:"id"`
		Members []struct {
			Code string `json:"code"`
		} `json:"members"`
	}
	if err := json.Unmarshal(raw, &dims); err != nil {
		t.Fatalf("decode /api/dimensions: %v: %s", err, raw)
	}
	for _, d := range dims {
		if d.ID == dimID {
			out := make([]string, 0, len(d.Members))
			for _, m := range d.Members {
				out = append(out, m.Code)
			}
			return out
		}
	}
	t.Fatalf("dimension %s not in /api/dimensions: %s", dimID, raw)
	return nil
}

// A member added by hand in Developer › Dimensions, or by the dimension CSV
// import, comes after the members already there — the connector and AI
// paths append MAX(sort_order)+1, and a hand-added member written with
// sort_order 0 jumped ahead of all of them.
func TestMemberCreatePathsAppend(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	const dev = "mm-dev"
	dimID := f.column(t, `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid`, f.modelA)[0]
	// A member a connector appended after MA.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid, 'CONN', 'From a connector', 5)`,
		dimID); err != nil {
		t.Fatal(err)
	}
	sortOrder := func(code string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx,
			`SELECT sort_order FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dimID, code).Scan(&n); err != nil {
			t.Fatalf("member %s: %v", code, err)
		}
		return n
	}

	if code, raw := f.do(t, "POST", "/api/developer/dimensions/"+dimID+"/members", dev, f.app1, f.modelA,
		map[string]any{"code": "HAND", "label": "Added by hand"}); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("add member: %d %s", code, raw)
	}
	if got := sortOrder("HAND"); got != 6 {
		t.Errorf("hand-added member sort_order = %d, want 6 (after the connector's 5)", got)
	}

	// The CSV import appends new codes and leaves a re-imported one where
	// it was.
	if code, raw := f.do(t, "POST", "/api/import/dimension-members", dev, f.app1, f.modelA,
		map[string]any{"dimension_id": dimID, "csv": "code,label\nCONN,Relabelled\nCSV1,First from CSV\nCSV2,Second from CSV\n"}); code != http.StatusOK {
		t.Fatalf("csv import: %d %s", code, raw)
	}
	for code, want := range map[string]int{"CONN": 5, "CSV1": 7, "CSV2": 8} {
		if got := sortOrder(code); got != want {
			t.Errorf("after the CSV import, %s sort_order = %d, want %d", code, got, want)
		}
	}

	code, raw := f.do(t, "GET", "/api/dimensions", dev, f.app1, f.modelA, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/dimensions: %d %s", code, raw)
	}
	if got, want := strings.Join(memberCodes(t, raw, dimID), ","), "MA,CONN,HAND,CSV1,CSV2"; got != want {
		t.Errorf("members in order %s, want %s", got, want)
	}
}

// GET /api/dimensions (form fields, the workflow inbox's pickers) orders
// members as the grid does: by period on a time dimension, not by code, so
// custom month codes read JAN, FEB, MAR rather than FEB, JAN, MAR.
func TestPublicDimensionsOrderTimeMembersByPeriod(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	var dimID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, 'Months', 'time', 'month', 1) RETURNING id::text`, f.modelA, f.revA).Scan(&dimID); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		code, start, end string
		idx              int
	}{
		{"FEB", "2026-02-01", "2026-02-28", 1},
		{"MAR", "2026-03-01", "2026-03-31", 2},
		{"JAN", "2026-01-01", "2026-01-31", 0},
	} {
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index)
			VALUES ($1::uuid, $2, $2, $3::date, $4::date, $5)`, dimID, m.code, m.start, m.end, m.idx); err != nil {
			t.Fatal(err)
		}
	}
	code, raw := f.do(t, "GET", "/api/dimensions", "mm-dev", f.app1, f.modelA, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/dimensions: %d %s", code, raw)
	}
	if got, want := strings.Join(memberCodes(t, raw, dimID), ","), "JAN,FEB,MAR"; got != want {
		t.Errorf("time members in order %s, want %s", got, want)
	}
}

// requireRevisionInModel, the guard for a revision_id in the request body,
// answers a malformed id with 404 and stores nothing — it used to cast the
// id to uuid, fail the query and answer 500.
func TestMalformedBodyRevisionIsNotFound(t *testing.T) {
	f := setupMMFixture(t)
	counts := func() string {
		var s string
		_ = f.pool.QueryRow(context.Background(), `SELECT
			(SELECT count(*) FROM model.grid_def)::text || '/' ||
			(SELECT count(*) FROM model.dimension_def)::text || '/' ||
			(SELECT count(*) FROM model.metric_def)::text || '/' ||
			(SELECT count(*) FROM model.dashboard_def)::text`).Scan(&s)
		return s
	}
	before := counts()
	for _, rev := range []string{"not-a-uuid", f.revD} {
		for _, tc := range []struct {
			path string
			body map[string]any
		}{
			{"/api/developer/grids", map[string]any{"name": "Stray"}},
			{"/api/developer/dimensions", map[string]any{"name": "Stray"}},
			{"/api/developer/metrics", map[string]any{"name": "stray", "is_input": true}},
			{"/api/developer/dashboards", map[string]any{"name": "Stray"}},
		} {
			tc.body["revision_id"] = rev
			if code, raw := f.do(t, "POST", tc.path, "mm-dev", f.app1, f.modelA, tc.body); code != http.StatusNotFound {
				t.Errorf("POST %s with body revision_id %q: status %d %s, want 404", tc.path, rev, code, raw)
			}
		}
	}
	if after := counts(); after != before {
		t.Errorf("refused creates stored rows: grids/dimensions/metrics/dashboards %s -> %s", before, after)
	}
}
