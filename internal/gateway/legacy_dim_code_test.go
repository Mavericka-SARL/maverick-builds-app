package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// No dimension name is special (standing rule 1). A model here has a
// dimension literally named "department" between "account" and "zone":
//
//   - the legacy single dim_code cell write goes to the one dimension of the
//     model and revision that has a member with that code — the code picks
//     the dimension, the name "department" does not; an ambiguous code, or
//     one no dimension has, answers 400 and points at dim_codes;
//   - the model-wide grid (no grid definition) orders dimensions by name,
//     as the grid-definition branch does, so "account" comes first.
func TestLegacyDimCodeAndGridOrderIgnoreDimensionNames(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('NamesCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Plan', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Plan model') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	otherRevID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Draft') RETURNING id::text`, modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID)
	metricID := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'spend', true, 'sum') RETURNING id::text`, modelID, revID)

	dim := func(rev, name string, codes ...string) string {
		id := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text`, modelID, rev, name)
		for _, c := range codes {
			exec(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $2)`, id, c)
		}
		return id
	}
	// Created department first, so neither creation order nor the old
	// name preference can pass for the name rule.
	deptID := dim(revID, "department", "D1", "SHARED")
	zoneID := dim(revID, "zone", "Z1")
	accountID := dim(revID, "account", "ACC1", "SHARED")
	// A code only another revision has, and one only another model has.
	dim(otherRevID, "region", "DRAFT_ONLY")
	otherModelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Other model') RETURNING id::text`, appID)
	otherModelDim := q(`INSERT INTO model.dimension_def (model_id, name) VALUES ($1::uuid, 'area') RETURNING id::text`, otherModelID)
	exec(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'ELSEWHERE', 'Elsewhere')`, otherModelDim)
	exec(`UPDATE core.application SET default_model_id=$1::uuid WHERE id=$2::uuid`, modelID, appID)

	userID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('names-alice', 'alice@namesco.test', 'alice', $1::uuid) RETURNING id::text`, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'business_user',$2::uuid)`, userID, wsID)

	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{License: enterpriseManager(t)}))
	t.Cleanup(srv.Close)

	do := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(buf))
		req.Header.Set("X-Dev-User", "names-alice")
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.Bytes()
	}
	legacyWrite := func(code string, value float64) (int, string) {
		t.Helper()
		status, body := do(http.MethodPost, "/api/cells", map[string]any{
			"model_id": modelID, "revision_id": revID, "metric_id": metricID, "dim_code": code, "value": value,
		})
		return status, string(body)
	}
	storedDims := func(value float64) map[string]string {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx,
			`SELECT dim_members::text FROM runtime.fact_input WHERE model_id=$1::uuid AND value=$2`, modelID, value,
		).Scan(&raw); err != nil {
			t.Fatalf("fact for value %v: %v", value, err)
		}
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("a unique code goes to the dimension that has it", func(t *testing.T) {
		for _, c := range []struct {
			code  string
			value float64
			dimID string
			dim   string
		}{
			{"Z1", 11, zoneID, "zone"},
			{"ACC1", 12, accountID, "account"},
			{"D1", 13, deptID, "department"},
		} {
			if status, body := legacyWrite(c.code, c.value); status != http.StatusOK {
				t.Fatalf("dim_code %s: %d %s", c.code, status, body)
			}
			if got := storedDims(c.value); len(got) != 1 || got[c.dimID] != c.code {
				t.Errorf("dim_code %s stored as %v, want {%s (%s): %s}", c.code, got, c.dimID, c.dim, c.code)
			}
		}
	})

	t.Run("an ambiguous or unknown code is refused and names dim_codes", func(t *testing.T) {
		for code, want := range map[string]string{
			"SHARED":     "more than one dimension",
			"NOPE":       "not a member of any dimension",
			"DRAFT_ONLY": "not a member of any dimension", // another revision's member
			"ELSEWHERE":  "not a member of any dimension", // another model's member
		} {
			status, body := legacyWrite(code, 99)
			if status != http.StatusBadRequest || !strings.Contains(body, want) || !strings.Contains(body, "dim_codes") {
				t.Errorf("dim_code %s: %d %s, want 400 %q pointing at dim_codes", code, status, body, want)
			}
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND value=99`, modelID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d refused writes were stored", n)
		}
		// Naming the dimension resolves the same code.
		if status, body := do(http.MethodPost, "/api/cells", map[string]any{
			"model_id": modelID, "revision_id": revID, "metric_id": metricID,
			"dim_codes": map[string]string{accountID: "SHARED"}, "value": 14,
		}); status != http.StatusOK {
			t.Fatalf("dim_codes SHARED: %d %s", status, body)
		}
		if got := storedDims(14); got[accountID] != "SHARED" {
			t.Errorf("dim_codes SHARED stored as %v", got)
		}
	})

	t.Run("the model-wide grid orders dimensions by name", func(t *testing.T) {
		status, body := do(http.MethodGet, "/api/grid?revision_id="+revID, nil)
		if status != http.StatusOK {
			t.Fatalf("grid: %d %s", status, body)
		}
		var g struct {
			Dimensions []struct {
				Name string `json:"name"`
			} `json:"dimensions"`
			Departments []struct {
				Code string `json:"code"`
			} `json:"departments"`
		}
		if err := json.Unmarshal(body, &g); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, d := range g.Dimensions {
			names = append(names, d.Name)
		}
		if strings.Join(names, ",") != "account,department,zone" {
			t.Errorf("dimension order %v, want [account department zone]", names)
		}
		// The compat "departments" list is the first dimension's members,
		// whatever that dimension is called.
		var codes []string
		for _, m := range g.Departments {
			codes = append(codes, m.Code)
		}
		if strings.Join(codes, ",") != "ACC1,SHARED" {
			t.Errorf("departments %v, want the first dimension's (account's) members [ACC1 SHARED]", codes)
		}
	})

	// A metric in a grid is dimensioned by that grid's dimensions — the ones
	// grid() keys its cells by — so the legacy code resolves among those.
	t.Run("a metric in a grid resolves among its grid's dimensions", func(t *testing.T) {
		gridMetricID := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'headcount', true, 'sum') RETURNING id::text`, modelID, revID)
		gridID := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'By department') RETURNING id::text`, modelID, revID)
		exec(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, gridID, gridMetricID)
		exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, gridID, deptID)
		write := func(code string, value float64) (int, string) {
			status, body := do(http.MethodPost, "/api/cells", map[string]any{
				"model_id": modelID, "revision_id": revID, "metric_id": gridMetricID, "dim_code": code, "value": value,
			})
			return status, string(body)
		}
		// SHARED is in department and account; only department is this
		// metric's, so it is not ambiguous here.
		if status, body := write("SHARED", 21); status != http.StatusOK {
			t.Fatalf("dim_code SHARED on a department-grid metric: %d %s", status, body)
		}
		if got := storedDims(21); len(got) != 1 || got[deptID] != "SHARED" {
			t.Errorf("dim_code SHARED stored as %v, want {%s (department): SHARED}", got, deptID)
		}
		// Z1 is only in zone, which the metric is not dimensioned by: a
		// write there would count in the total but show in no cell.
		status, body := write("Z1", 98)
		if status != http.StatusBadRequest || !strings.Contains(body, "not a member of any dimension of this metric's grids") || !strings.Contains(body, "dim_codes") {
			t.Errorf("dim_code Z1 on a department-grid metric: %d %s, want 400 pointing at dim_codes", status, body)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND value=98`, modelID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d refused writes were stored", n)
		}
	})
}
