// Dimensional formulas over the developer's HTTP API (contract C1–C3,
// C8–C10): a developer declares typed member properties, sets their values,
// saves metrics that read them (region.factor, LOOKUP, SUMIFS, COUNTIFS),
// is refused with the right codes for bad formulas and bad declarations,
// and sees dependent metrics recalculated when members and properties
// change — a property rename included.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type dimFormulaFixture struct {
	t                     *testing.T
	pool                  *pgxpool.Pool
	srv                   *httptest.Server
	appID, modelID, revID string
	dev                   string
	region, gridID        string
	members               map[string]string // region code -> member id
	metric                map[string]string // name -> id
}

func (f *dimFormulaFixture) req(method, path string, body any) (int, []byte) {
	f.t.Helper()
	var buf []byte
	if body != nil {
		var err error
		if buf, err = json.Marshal(body); err != nil {
			f.t.Fatalf("encode body: %v", err)
		}
	}
	r, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	r.Header.Set("X-Dev-User", f.dev)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-App-Id", f.appID)
	r.Header.Set("X-Revision-Id", f.revID)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		f.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	out := new(bytes.Buffer)
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

func (f *dimFormulaFixture) call(method, path string, body any) string {
	f.t.Helper()
	status, raw := f.req(method, path, body)
	if status < 200 || status >= 300 {
		f.t.Fatalf("%s %s: status %d\n%s", method, path, status, raw)
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	id, _ := parsed["id"].(string)
	return id
}

// refused asserts a 400 whose message carries code.
func (f *dimFormulaFixture) refused(what, method, path string, body any, code string) {
	f.t.Helper()
	status, raw := f.req(method, path, body)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), code) {
		f.t.Errorf("%s: status %d body %s; want 400 with %s", what, status, raw, code)
	}
}

func (f *dimFormulaFixture) metricBody(name, text string) map[string]any {
	return map[string]any{"name": name, "is_input": text == "", "formula": text,
		"revision_id": f.revID, "agg_rule": "sum", "format": "number"}
}

func (f *dimFormulaFixture) setMember(code string, props map[string]string) {
	f.t.Helper()
	f.call("PATCH", "/api/developer/dimensions/"+f.region+"/members/"+f.members[code],
		map[string]any{"code": code, "label": code, "properties": props})
}

func (f *dimFormulaFixture) writeCell(metric, region string, v float64) {
	f.t.Helper()
	f.call("POST", "/api/cells", map[string]any{
		"model_id": f.modelID, "metric_id": f.metric[metric], "revision_id": f.revID,
		"dim_codes": map[string]string{f.region: region}, "value": v,
	})
}

// await polls calc_result: every recalculation here runs in a goroutine.
func (f *dimFormulaFixture) await(metric, region string, want float64) {
	f.t.Helper()
	raw, _ := json.Marshal(map[string]string{f.region: region})
	deadline := time.Now().Add(15 * time.Second)
	var last float64
	seen := false
	for time.Now().Before(deadline) {
		var v float64
		err := f.pool.QueryRow(context.Background(), `
			SELECT value FROM runtime.calc_result
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members=$4::jsonb
		`, f.modelID, f.revID, f.metric[metric], string(raw)).Scan(&v)
		if err == nil {
			last, seen = v, true
			if nearly(v, want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !seen {
		f.t.Errorf("%s at %s: no calc_result row (want %v)", metric, region, want)
		return
	}
	f.t.Errorf("%s at %s = %v, want %v (waited 15s)", metric, region, last, want)
}

func setupDimFormulaFixture(t *testing.T) *dimFormulaFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	f := &dimFormulaFixture{t: t, pool: pool, dev: "dimf-dev", members: map[string]string{}, metric: map[string]string{}}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	// Tenant scaffolding only; the model itself is built over HTTP below.
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('DimF Co', 'enterprise') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'W') RETURNING id::text`, cust)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'A', 'planning') RETURNING id::text`, ws, cust)
	f.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, f.appID)
	f.revID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelID)
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, f.revID, f.modelID); err != nil {
		t.Fatal(err)
	}
	uid := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,'dimf@x.co','Dev',$2::uuid) RETURNING id::text`, f.dev, cust)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, uid, ws); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)

	f.region = f.call("POST", "/api/developer/dimensions", map[string]any{"name": "region", "revision_id": f.revID, "dimension_type": "standard"})
	for _, code := range []string{"EMEA", "US"} {
		f.members[code] = f.call("POST", "/api/developer/dimensions/"+f.region+"/members", map[string]any{"code": code, "label": code})
	}
	f.metric["revenue"] = f.call("POST", "/api/developer/metrics", f.metricBody("revenue", ""))
	f.gridID = f.call("POST", "/api/developer/grids", map[string]any{"name": "Plan", "revision_id": f.revID})
	f.call("POST", "/api/developer/grids/"+f.gridID+"/dimensions/"+f.region, nil)
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["revenue"], nil)
	return f
}

func TestDimensionalFormulasOverHTTP(t *testing.T) {
	f := setupDimFormulaFixture(t)
	props := "/api/developer/dimensions/" + f.region + "/properties"

	// Property declarations (C9): a name formulas can write, a known type,
	// unique regardless of case.
	f.call("POST", props, map[string]any{"name": "segment", "data_type": "text"})
	factorProp := f.call("POST", props, map[string]any{"name": "factor", "data_type": "number"})
	f.refused("a name with a space", "POST", props, map[string]any{"name": "Sales Segment", "data_type": "text"}, metricformula.CodeInvalidPropertyName)
	f.refused("a name starting with a digit", "POST", props, map[string]any{"name": "2nd", "data_type": "text"}, metricformula.CodeInvalidPropertyName)
	f.refused("an unknown type", "POST", props, map[string]any{"name": "tier", "data_type": "money"}, metricformula.CodeInvalidPropertyType)
	f.refused("a name differing only in case", "POST", props, map[string]any{"name": "SEGMENT", "data_type": "text"}, metricformula.CodePropertyNameTaken)
	f.refused("a rename to a bad name", "PATCH", props+"/"+factorProp, map[string]any{"name": "fac-tor"}, metricformula.CodeInvalidPropertyName)

	f.setMember("EMEA", map[string]string{"segment": "SMB", "factor": "2"})
	f.setMember("US", map[string]string{"segment": "ENT", "factor": "3"})
	f.writeCell("revenue", "EMEA", 100)
	f.writeCell("revenue", "US", 50)

	// Bad formulas are refused at save with their codes (C10).
	for _, tc := range []struct{ text, code string }{
		{`revenue * region.colour`, formula.CodeUnknownProperty},
		{`LOOKUP(revenue, region, "Atlantis")`, formula.CodeUnknownMember},
		{`LOOKUP(region, region, "EMEA")`, formula.CodeSourceMustBeMetric},
		{`SUMIFS(revenue, revenue, "x")`, formula.CodeDimensionArgRequired},
		{`PARENT(revenue)`, formula.CodeDimensionArgRequired},
	} {
		f.refused(tc.text, "POST", "/api/developer/metrics", f.metricBody("bad", tc.text), tc.code)
	}

	// Metrics reading the dimension save and compute.
	for name, text := range map[string]string{
		"scaled":    `revenue * region.factor`,
		"smb":       `SUMIFS(revenue, region.segment, "SMB")`,
		"emea":      `LOOKUP(revenue, region, "EMEA")`,
		"n_regions": `COUNTIFS(region, "*")`,
	} {
		f.metric[name] = f.call("POST", "/api/developer/metrics", f.metricBody(name, text))
		f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric[name], nil)
	}
	// Placing the metrics recalculates nothing by itself; a cell edit does.
	f.writeCell("revenue", "EMEA", 100)
	f.await("scaled", "EMEA", 200)
	f.await("scaled", "US", 150)
	f.await("smb", "US", 100)
	f.await("emea", "US", 100)
	f.await("n_regions", "US", 2)

	// C8: editing a member's properties recalculates the metrics reading them.
	f.setMember("EMEA", map[string]string{"factor": "5"})
	f.await("scaled", "EMEA", 500)
	f.setMember("US", map[string]string{"segment": "SMB"})
	f.await("smb", "EMEA", 150)

	// C8: a new member is seen by COUNTIFS without any cell edit.
	f.members["APAC"] = f.call("POST", "/api/developer/dimensions/"+f.region+"/members", map[string]any{"code": "APAC", "label": "APAC"})
	f.await("n_regions", "US", 3)

	// C8: renaming a property renames every member's value with it.
	f.call("PATCH", props+"/"+factorProp, map[string]any{"name": "multiplier"})
	var oldKeys, newKeys int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FILTER (WHERE properties ? 'factor'), COUNT(*) FILTER (WHERE properties ? 'multiplier')
		FROM model.dimension_member WHERE dimension_id=$1::uuid`, f.region).Scan(&oldKeys, &newKeys); err != nil {
		t.Fatal(err)
	}
	if oldKeys != 0 || newKeys != 2 {
		t.Errorf("after renaming factor to multiplier: %d members keep factor, %d carry multiplier; want 0 and 2", oldKeys, newKeys)
	}
	var dataType string
	if err := f.pool.QueryRow(context.Background(), `SELECT data_type FROM model.dimension_property WHERE id=$1::uuid`, factorProp).Scan(&dataType); err != nil {
		t.Fatal(err)
	}
	if dataType != "number" {
		t.Errorf("a rename that sent no data_type changed it to %q", dataType)
	}
	// The old formula no longer saves; renamed, it computes from the
	// migrated values.
	f.refused("the old property name", "PATCH", "/api/developer/metrics/"+f.metric["scaled"],
		map[string]any{"name": "scaled", "formula": `revenue * region.factor`, "agg_rule": "sum", "format": "number"}, formula.CodeUnknownProperty)
	f.call("PATCH", "/api/developer/metrics/"+f.metric["scaled"],
		map[string]any{"name": "scaled", "formula": `revenue * region.multiplier`, "agg_rule": "sum", "format": "number"})
	f.writeCell("revenue", "US", 60)
	f.await("scaled", "EMEA", 500)
	f.await("scaled", "US", 180)

	// C8: a CSV member import recalculates the metrics reading the
	// imported properties.
	f.call("POST", "/api/import/dimension-members", map[string]any{
		"dimension_id": f.region, "csv": "code,label,property:multiplier\nEMEA,EMEA,7\n",
	})
	f.await("scaled", "EMEA", 700)

	// Removing region from revenue's grid would leave LOOKUP and SUMIFS
	// reading revenue along a dimension it no longer has: refused, as the
	// add is checked.
	f.refused("removing a dimension a LOOKUP reads its source along", "DELETE",
		"/api/developer/grids/"+f.gridID+"/dimensions/"+f.region, nil, formula.CodeDimensionNotOnSource)
}
