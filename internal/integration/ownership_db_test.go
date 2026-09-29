package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// foreignTenant seeds a second customer with its own model, a dimension
// holding a member, and a form holding a record: the rows a connector in
// the first tenant must never read or write.
func foreignTenant(t *testing.T, st *integration.Store) (dimID, formID string) {
	t.Helper()
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
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('OtherCo','enterprise') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws2') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App2','planning') RETURNING id::text`, ws, cust)
	model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M2') RETURNING id::text`, app)
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, model)
	user := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('other-user','o@other.co','O',$1::uuid) RETURNING id::text`, cust)
	dimID = q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid,'accounts',$2::uuid) RETURNING id::text`, model, rev)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'SECRET-ACCT','Secret account') RETURNING id::text`, dimID)
	formID = q(`INSERT INTO model.form_def (model_id, name, label, revision_id) VALUES ($1::uuid,'payroll','Payroll',$2::uuid) RETURNING id::text`, model, rev)
	q(`INSERT INTO runtime.form_record (form_id, data, status, created_by) VALUES ($1::uuid,'{"salary":"SECRET-SALARY"}','submitted',$2::uuid) RETURNING id::text`, formID, user)
	return dimID, formID
}

// A stored connector whose target is another tenant's row (saved before the
// save-time check, or by a path that skipped it) must fail its run before
// any request is made or any row is read or written. Before the worker
// checked ownership, the pull wrote a member into the foreign dimension and
// the push sent the foreign form's records out.
func TestRunner_ForeignTargetFailsTheRun(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	rn, st, modelID, revID, _ := setupRunner(t)
	ctx := context.Background()
	foreignDim, foreignForm := foreignTenant(t, st)

	var hits atomic.Int32
	var sent atomic.Value
	sent.Store("")
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		buf := new(strings.Builder)
		b := make([]byte, 4096)
		for {
			n, rerr := r.Body.Read(b)
			buf.Write(b[:n])
			if rerr != nil {
				break
			}
		}
		sent.Store(sent.Load().(string) + buf.String())
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"code":"PWNED","label":"planted"}]}`)
	}))
	defer fixture.Close()

	pullDim := func(targetID string) *integration.Definition {
		t.Helper()
		cfg := &integration.Config{
			Kind: integration.ConfigKind, Direction: integration.DirectionPull,
			TargetType: integration.TargetDimension, TargetID: targetID,
			ImportMode: integration.ModeIncremental,
			Request:    integration.RequestConfig{Method: "GET", URL: fixture.URL + "/members", BodyMode: integration.BodyNone},
			Auth:       integration.AuthPlacement{Type: "none"},
			Response:   integration.ResponseConfig{Format: integration.FormatJSON, RecordsPath: "$.items"},
			Pagination: integration.PaginationConfig{Mode: integration.PageNone},
			Mapping: integration.MappingConfig{Fields: []integration.FieldMap{
				{Source: "$.code", Target: "code"},
				{Source: "$.label", Target: "label"},
			}},
		}
		def, err := st.CreateDefinition(ctx, modelID, revID, "pull "+targetID[:8], "", nil, "draft", "", cfg, true)
		if err != nil {
			t.Fatalf("create def: %v", err)
		}
		return def
	}
	memberCount := func(dimID, code string) int {
		t.Helper()
		var n int
		if err := st.Pool().QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dimID, code).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("pull into a foreign dimension", func(t *testing.T) {
		hits.Store(0)
		run := claimAndRun(t, rn, st, pullDim(foreignDim).ID, "manual", false)
		if run.Status != "failed" || run.ErrorCode != integration.ErrCodeInvalidData || !strings.Contains(run.Message, "different model") {
			t.Fatalf("run = %s/%s %q, want failed/invalid_data naming the foreign target", run.Status, run.ErrorCode, run.Message)
		}
		if n := memberCount(foreignDim, "PWNED"); n != 0 {
			t.Fatalf("the run wrote %d member(s) into another tenant's dimension", n)
		}
		if hits.Load() != 0 {
			t.Fatalf("the run made %d request(s) before failing", hits.Load())
		}
	})

	t.Run("push from a foreign form", func(t *testing.T) {
		hits.Store(0)
		sent.Store("")
		cfg := &integration.Config{
			Kind: integration.ConfigKind, Direction: integration.DirectionPush,
			TargetType: integration.TargetForm, TargetID: foreignForm,
			Request: integration.RequestConfig{
				Method: "POST", URL: fixture.URL + "/ingest", BodyMode: integration.BodyJSON,
				BodyJSON: `{"salary":"{{row.salary}}"}`,
			},
			Auth:    integration.AuthPlacement{Type: "none"},
			Mapping: integration.MappingConfig{Fields: []integration.FieldMap{{Source: "salary", Target: "salary"}}},
		}
		def, err := st.CreateDefinition(ctx, modelID, revID, "push foreign form", "", nil, "draft", "", cfg, true)
		if err != nil {
			t.Fatalf("create def: %v", err)
		}
		run := claimAndRun(t, rn, st, def.ID, "manual", false)
		if run.Status != "failed" || run.ErrorCode != integration.ErrCodeInvalidData {
			t.Fatalf("run = %s/%s %q, want failed/invalid_data", run.Status, run.ErrorCode, run.Message)
		}
		if hits.Load() != 0 || strings.Contains(sent.Load().(string), "SECRET-SALARY") {
			t.Fatalf("another tenant's form record left the process: %d request(s), body %q", hits.Load(), sent.Load())
		}
	})

	t.Run("own dimension still pulls", func(t *testing.T) {
		var ownDim string
		if err := st.Pool().QueryRow(ctx, `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='region'`, modelID).Scan(&ownDim); err != nil {
			t.Fatal(err)
		}
		run := claimAndRun(t, rn, st, pullDim(ownDim).ID, "manual", false)
		if run.Status != "success" || run.RecordsWritten != 1 {
			t.Fatalf("own-model pull: %+v", run)
		}
		if memberCount(ownDim, "PWNED") != 1 {
			t.Fatal("own-model pull did not write its member")
		}
	})
}

// The committer is the last line before a write or an outbound read, and
// checks ownership itself, whoever calls it.
func TestDBCommitter_RefusesForeignTarget(t *testing.T) {
	st, _, modelID, revID := setupStore(t)
	ctx := context.Background()
	foreignDim, foreignForm := foreignTenant(t, st)
	c := &integration.DBCommitter{Pool: st.Pool(), Log: logger.New("test")}

	dimDef := &integration.Definition{ModelID: modelID, RevisionID: revID,
		Config: &integration.Config{TargetType: integration.TargetDimension, TargetID: foreignDim}}
	written, _, err := c.CommitPull(ctx, dimDef, []string{"code", "label"}, [][]string{{"PWNED", "planted"}}, false, "dev-user")
	if err == nil || written != 0 {
		t.Fatalf("CommitPull into a foreign dimension: written=%d err=%v", written, err)
	}
	var n int
	_ = st.Pool().QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='PWNED'`, foreignDim).Scan(&n)
	if n != 0 {
		t.Fatalf("%d member(s) written into another tenant's dimension", n)
	}

	for _, tc := range []struct {
		typ integration.TargetType
		id  string
	}{{integration.TargetForm, foreignForm}, {integration.TargetDimension, foreignDim}} {
		def := &integration.Definition{ModelID: modelID, RevisionID: revID,
			Config: &integration.Config{TargetType: tc.typ, TargetID: tc.id}}
		rows, err := c.LoadPushRows(ctx, def)
		if err == nil || len(rows) != 0 {
			t.Fatalf("LoadPushRows from a foreign %s: %d row(s), err=%v", tc.typ, len(rows), err)
		}
	}
}

// CheckOwnership is the one rule every path shares.
func TestCheckOwnership(t *testing.T) {
	st, appID, modelID, revID := setupStore(t)
	ctx := context.Background()
	pool := st.Pool()
	foreignDim, _ := foreignTenant(t, st)
	var ownGrid, foreignApp string
	if err := pool.QueryRow(ctx, `INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid,'G',$2::uuid) RETURNING id::text`, modelID, revID).Scan(&ownGrid); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT a.id::text FROM core.application a WHERE a.name='App2'`).Scan(&foreignApp)
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	ownConn, err := st.CreateConnection(ctx, appID, "own", "none", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	foreignConn, err := st.CreateConnection(ctx, foreignApp, "theirs", "none", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name                   string
		targetType, target, cn string
		wantErr                string
		foreign                bool
	}{
		{"own grid, own connection", "grid", ownGrid, ownConn.ID, "", false},
		{"draft without target or connection", "grid", "", "", "", false},
		{"foreign dimension", "dimension", foreignDim, "", "different model", true},
		{"foreign id under another type", "grid", foreignDim, "", "not found", false},
		{"not a uuid", "form", "abc", "", "not found", false},
		{"unknown type", "sheet", ownGrid, "", "unknown target type", false},
		{"foreign connection", "grid", ownGrid, foreignConn.ID, "different application", true},
		{"missing connection", "grid", ownGrid, "00000000-0000-0000-0000-000000000000", "connection not found", false},
	} {
		err := integration.CheckOwnership(ctx, pool, modelID, tc.targetType, tc.target, tc.cn)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
		}
		// Only a row that exists elsewhere is "foreign"; a missing one is
		// what a deleted target looks like.
		if got := integration.IsForeign(err); got != tc.foreign {
			t.Errorf("%s: IsForeign = %v, want %v", tc.name, got, tc.foreign)
		}
	}
}
