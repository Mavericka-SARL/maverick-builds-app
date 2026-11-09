package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A pull into a grid maps every dimension of the grid, as a file run must
// (importpkg.CheckGridColumns): a record naming a region but no scenario on a
// region x scenario grid was stored without the scenario — where no cell and
// no formula reads it — and the run reported success.
func TestRunner_PullRefusedWhenAGridDimensionIsUnmapped(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	rn, st, modelID, revID, gridID := setupRunner(t)
	ctx := context.Background()
	pool := st.Pool()
	var scenario string
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid,'scenario',$2::uuid) RETURNING id::text`,
		modelID, revID).Scan(&scenario); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'Base','Base')`, scenario); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid)`, gridID, scenario); err != nil {
		t.Fatal(err)
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"items":[{"region":"A","amount":10}],"next":null}}`)
	}))
	defer fixture.Close()

	def := mkPullDef(t, st, modelID, revID, gridID, fixture.URL+"/v1/items", nil)
	run := claimAndRun(t, rn, st, def.ID, "manual", false)
	if run.Status == "success" {
		t.Errorf("run with scenario unmapped: %+v, want it failed", run)
	}
	var facts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, modelID).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 0 {
		t.Errorf("%d facts stored without the grid's scenario", facts)
	}
}
