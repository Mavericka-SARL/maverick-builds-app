package gateway

// Who may read, change and delete a form record ("submitter + admins",
// crudapp.RecordAccess): PUT and DELETE /api/records/{id} used to be open to
// every signed-in user for any record id, of any workspace or tenant, and a
// status change fired the form's form_submit/form_approval rules. These
// tests drive the running handler over HTTP as the real roles: two tenants,
// two workspaces in the first, one application in its first workspace.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// Personas are keycloak_subs (resolveDevActor takes one as X-Dev-User).
const (
	prOwner      = "rec-owner"       // business_user of the application's workspace
	prPeer       = "rec-peer"        // another business_user of that workspace
	prBA         = "rec-ba"          // business_admin of that workspace
	prBAOtherWS  = "rec-ba-other-ws" // business_admin of the tenant's other workspace
	prDev        = "rec-dev"         // developer, in the tenant's other workspace
	prTA         = "rec-ta"          // tenant_admin of the tenant
	prForeign    = "rec-foreign"     // business_user of another tenant
	prForeignBA  = "rec-foreign-ba"  // business_admin of another tenant
	prForeignDev = "rec-foreign-dev" // developer of another tenant
)

type recordFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	srv    *httptest.Server
	appID  string
	formID string
	userID map[string]string
	// custA is the first tenant; modelID and revID the application's model
	// and its active revision.
	custA, modelID, revID string
}

func setupRecordFixture(t *testing.T) *recordFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	f := &recordFixture{t: t, pool: pool, userID: map[string]string{}}

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	// Tenant scaffolding straight into the database, as the sales demo
	// fixture does; the form and every record go through the API.
	custA := q(`INSERT INTO core.customer (name, plan) VALUES ('Tenant A', 'enterprise') RETURNING id::text`)
	custB := q(`INSERT INTO core.customer (name, plan) VALUES ('Tenant B', 'enterprise') RETURNING id::text`)
	wsA1 := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'A1') RETURNING id::text`, custA)
	wsA2 := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'A2') RETURNING id::text`, custA)
	wsB := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'B') RETURNING id::text`, custB)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Expenses', 'planning') RETURNING id::text`, wsA1, custA)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, f.appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	f.custA, f.modelID, f.revID = custA, modelID, revID
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID); err != nil {
		t.Fatalf("activate revision: %v", err)
	}
	// Tenant B has an application of its own, so its people open something.
	appB := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'B app', 'planning') RETURNING id::text`, wsB, custB)
	q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'MB') RETURNING id::text`, appB)

	mk := func(sub, cust, role, ws string) {
		uid := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1 || '@t.co', $1, $2::uuid) RETURNING id::text`, sub, cust)
		var wsArg any
		if ws != "" {
			wsArg = ws
		}
		if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, $3::uuid)`, uid, role, wsArg); err != nil {
			t.Fatalf("grant %s %s: %v", sub, role, err)
		}
		f.userID[sub] = uid
	}
	mk(prOwner, custA, "business_user", wsA1)
	mk(prPeer, custA, "business_user", wsA1)
	mk(prBA, custA, "business_admin", wsA1)
	mk(prBAOtherWS, custA, "business_admin", wsA2)
	mk(prDev, custA, "developer", wsA2)
	mk(prTA, custA, "tenant_admin", "")
	mk(prForeign, custB, "business_user", wsB)
	mk(prForeignBA, custB, "business_admin", wsB)
	mk(prForeignDev, custB, "developer", wsB)

	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)

	// The form is a developer's, made the developer's way.
	status, body := f.do(http.MethodPost, "/api/forms", prDev, map[string]any{
		"name": "expense", "label": "Expense",
		"fields": []map[string]any{{"name": "amount", "label": "Amount", "type": "number"}},
	})
	if status != http.StatusOK {
		t.Fatalf("create form as developer: %d %s", status, body)
	}
	f.formID = decodeObj(t, body)["id"].(string)
	return f
}

func (f *recordFixture) do(method, path, persona string, body any) (int, []byte) {
	f.t.Helper()
	return f.doIn(f.appID, method, path, persona, body)
}

// doIn sends the request with appID as X-App-Id.
func (f *recordFixture) doIn(appID, method, path, persona string, body any) (int, []byte) {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			f.t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, &buf)
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", persona)
	req.Header.Set("X-App-Id", appID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

// expect sends the request and fails unless it answers want.
func (f *recordFixture) expect(want int, method, path, persona string, body any) []byte {
	f.t.Helper()
	status, raw := f.do(method, path, persona, body)
	if status != want {
		f.t.Errorf("%s %s as %s: status %d, want %d: %s", method, path, persona, status, want, raw)
	}
	return raw
}

// create makes a record as persona with status, through the API.
func (f *recordFixture) create(persona, status string, amount float64) string {
	f.t.Helper()
	raw := f.expect(http.StatusOK, http.MethodPost, "/api/forms/"+f.formID+"/records", persona,
		map[string]any{"data": map[string]any{"amount": amount}, "status": status})
	id, _ := decodeObj(f.t, raw)["id"].(string)
	if id == "" {
		f.t.Fatalf("create record as %s: no id in %s", persona, raw)
	}
	return id
}

// dbRecord reads a record's status and amount straight from the table.
func (f *recordFixture) dbRecord(id string) (status string, amount float64, exists bool) {
	f.t.Helper()
	err := f.pool.QueryRow(context.Background(),
		`SELECT status::text, COALESCE((data->>'amount')::float8, 0) FROM runtime.form_record WHERE id=$1::uuid`, id,
	).Scan(&status, &amount)
	if err != nil {
		return "", 0, false
	}
	return status, amount, true
}

func decodeObj(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

type servedPerms struct {
	Edit      bool     `json:"edit"`
	Delete    bool     `json:"delete"`
	SetStatus []string `json:"set_status"`
}

type servedRecord struct {
	ID          string       `json:"id"`
	Status      string       `json:"status"`
	CreatedBy   *string      `json:"created_by"`
	Permissions *servedPerms `json:"permissions"`
}

func perms(edit, del bool, set ...string) servedPerms {
	if set == nil {
		set = []string{}
	}
	return servedPerms{Edit: edit, Delete: del, SetStatus: set}
}

func checkPerms(t *testing.T, label string, got *servedPerms, want servedPerms) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: no permissions served", label)
		return
	}
	g := *got
	sort.Strings(g.SetStatus)
	sort.Strings(want.SetStatus)
	if g.SetStatus == nil {
		g.SetStatus = []string{}
	}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("%s: permissions %+v, want %+v", label, g, want)
	}
}

// TestFormRecordOutsideReachIs404 — a record of an application the caller
// does not open, in another tenant or another workspace of the same tenant,
// does not exist for them: every record route answers 404 and changes
// nothing.
func TestFormRecordOutsideReachIs404(t *testing.T) {
	f := setupRecordFixture(t)
	rec := f.create(prOwner, "submitted", 10)
	recPath := "/api/records/" + rec
	formPath := "/api/forms/" + f.formID

	for _, p := range []string{prForeign, prForeignBA, prForeignDev, prBAOtherWS} {
		f.expect(http.StatusNotFound, http.MethodGet, recPath, p, nil)
		f.expect(http.StatusNotFound, http.MethodPut, recPath, p, map[string]any{"status": "approved"})
		f.expect(http.StatusNotFound, http.MethodPut, recPath, p, map[string]any{"data": map[string]any{"amount": 666}})
		f.expect(http.StatusNotFound, http.MethodDelete, recPath, p, nil)
		f.expect(http.StatusNotFound, http.MethodGet, formPath+"/records", p, nil)
		f.expect(http.StatusNotFound, http.MethodPost, formPath+"/records", p, map[string]any{"data": map[string]any{"amount": 1}})
		f.expect(http.StatusNotFound, http.MethodGet, formPath+"/export?format=csv", p, nil)
		f.expect(http.StatusNotFound, http.MethodPost, formPath+"/import", p, map[string]string{"csv": "amount\n5\n"})
		f.expect(http.StatusNotFound, http.MethodPost, formPath+"/sync", p, nil)
	}
	// A record id that does not exist answers the same.
	f.expect(http.StatusNotFound, http.MethodGet, "/api/records/00000000-0000-0000-0000-00000000abcd", prBA, nil)
	f.expect(http.StatusNotFound, http.MethodPut, "/api/records/not-a-uuid", prBA, map[string]any{"status": "approved"})

	status, amount, exists := f.dbRecord(rec)
	if !exists || status != "submitted" || amount != 10 {
		t.Errorf("record after the refused calls: exists=%v status=%q amount=%v, want submitted/10", exists, status, amount)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, f.formID).Scan(&n); err != nil || n != 1 {
		t.Errorf("records in the form = %d (err %v), want 1: a create or import from outside got through", n, err)
	}
}

// TestFormRecordSubmitterAndAdmins walks the rule through the real roles.
func TestFormRecordSubmitterAndAdmins(t *testing.T) {
	f := setupRecordFixture(t)
	rec := f.create(prOwner, "submitted", 10)
	path := "/api/records/" + rec

	// The creator edits their own submitted record, and moves it between
	// draft and submitted.
	f.expect(http.StatusOK, http.MethodPut, path, prOwner, map[string]any{"data": map[string]any{"amount": 11}, "status": "submitted"})
	f.expect(http.StatusOK, http.MethodPut, path, prOwner, map[string]any{"data": map[string]any{"amount": 12}, "status": "draft"})
	f.expect(http.StatusOK, http.MethodPut, path, prOwner, map[string]any{"data": map[string]any{"amount": 12}, "status": "submitted"})
	// ...but never decides it.
	f.expect(http.StatusForbidden, http.MethodPut, path, prOwner, map[string]any{"data": map[string]any{"amount": 12}, "status": "approved"})
	f.expect(http.StatusForbidden, http.MethodPut, path, prOwner, map[string]any{"status": "rejected"})
	// Creating a decided record is an administrator's too.
	f.expect(http.StatusForbidden, http.MethodPost, "/api/forms/"+f.formID+"/records", prOwner,
		map[string]any{"data": map[string]any{"amount": 1}, "status": "approved"})

	// Another business user of the workspace reads it, and nothing more.
	raw := f.expect(http.StatusOK, http.MethodGet, path, prPeer, nil)
	var seen servedRecord
	_ = json.Unmarshal(raw, &seen)
	checkPerms(t, "peer on a submitted record", seen.Permissions, perms(false, false))
	f.expect(http.StatusForbidden, http.MethodPut, path, prPeer, map[string]any{"data": map[string]any{"amount": 99}, "status": "submitted"})
	f.expect(http.StatusForbidden, http.MethodPut, path, prPeer, map[string]any{"status": "draft"})
	f.expect(http.StatusForbidden, http.MethodDelete, path, prPeer, nil)

	if status, amount, _ := f.dbRecord(rec); status != "submitted" || amount != 12 {
		t.Fatalf("record after the creator's and peer's calls: %s/%v, want submitted/12", status, amount)
	}

	// The workspace's business admin approves it.
	raw = f.expect(http.StatusOK, http.MethodPut, path, prBA, map[string]any{"data": map[string]any{"amount": 12}, "status": "approved"})
	var put struct {
		Record servedRecord `json:"record"`
	}
	_ = json.Unmarshal(raw, &put)
	if put.Record.Status != "approved" {
		t.Errorf("PUT response record status %q, want approved", put.Record.Status)
	}
	checkPerms(t, "business admin's PUT response", put.Record.Permissions, perms(true, true, "draft", "submitted", "rejected"))

	// Decided, the record is out of its creator's hands.
	f.expect(http.StatusForbidden, http.MethodPut, path, prOwner, map[string]any{"data": map[string]any{"amount": 1}, "status": "approved"})
	f.expect(http.StatusForbidden, http.MethodPut, path, prOwner, map[string]any{"data": map[string]any{"amount": 12}, "status": "draft"})
	f.expect(http.StatusForbidden, http.MethodDelete, path, prOwner, nil)
	if status, amount, _ := f.dbRecord(rec); status != "approved" || amount != 12 {
		t.Fatalf("approved record after its creator's calls: %s/%v, want approved/12", status, amount)
	}

	// The business admin deletes it.
	f.expect(http.StatusOK, http.MethodDelete, path, prBA, nil)
	if _, _, exists := f.dbRecord(rec); exists {
		t.Errorf("record still exists after the business admin deleted it")
	}

	// The creator deletes their own draft.
	own := f.create(prOwner, "draft", 3)
	f.expect(http.StatusOK, http.MethodDelete, "/api/records/"+own, prOwner, nil)

	// A developer (a role held in the tenant's other workspace) and the
	// tenant admin administer the application's records too.
	for _, admin := range []string{prDev, prTA} {
		r := f.create(prOwner, "submitted", 5)
		f.expect(http.StatusOK, http.MethodPut, "/api/records/"+r, admin, map[string]any{"status": "approved"})
		if status, amount, _ := f.dbRecord(r); status != "approved" || amount != 5 {
			t.Errorf("%s approval: record %s/%v, want approved/5 (fields kept)", admin, status, amount)
		}
		f.expect(http.StatusOK, http.MethodDelete, "/api/records/"+r, admin, nil)
	}

	// The audit trail names who changed the status from what, and who deleted.
	var prev, actor string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT COALESCE(metadata->>'previous_status', ''), COALESCE(actor_user_id::text, '') FROM audit.audit_event
		WHERE event_type='form_record.updated' AND resource_id=$1 AND metadata->>'status'='approved'
		ORDER BY occurred_at DESC LIMIT 1`, rec).Scan(&prev, &actor); err != nil {
		t.Errorf("no form_record.updated audit event for the approval: %v", err)
	} else if prev != "submitted" || actor != f.userID[prBA] {
		t.Errorf("approval audit: previous_status %q actor %s, want submitted by %s", prev, actor, f.userID[prBA])
	}
	var deleted int
	_ = f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit.audit_event WHERE event_type='form_record.deleted' AND resource_id=$1 AND actor_user_id=$2::uuid`,
		rec, f.userID[prBA]).Scan(&deleted)
	if deleted != 1 {
		t.Errorf("form_record.deleted audit events for the business admin's delete = %d, want 1", deleted)
	}
}

// TestFormRecordListCarriesCallerPermissions — every record the list
// returns carries its creator and what THIS caller may do to it.
func TestFormRecordListCarriesCallerPermissions(t *testing.T) {
	f := setupRecordFixture(t)
	ownDraft := f.create(prOwner, "draft", 1)
	ownApproved := f.create(prOwner, "submitted", 2)
	f.expect(http.StatusOK, http.MethodPut, "/api/records/"+ownApproved, prBA, map[string]any{"status": "approved"})
	peerSubmitted := f.create(prPeer, "submitted", 3)
	// A legacy record whose creator was never recorded (migration 094 made
	// created_by nullable when users are deleted).
	var legacy string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO runtime.form_record (form_id, data, status, created_by) VALUES ($1::uuid, '{"amount":4}', 'submitted', NULL) RETURNING id::text`,
		f.formID).Scan(&legacy); err != nil {
		t.Fatalf("insert legacy record: %v", err)
	}

	list := func(persona string) map[string]servedRecord {
		t.Helper()
		raw := f.expect(http.StatusOK, http.MethodGet, "/api/forms/"+f.formID+"/records", persona, nil)
		var recs []servedRecord
		if err := json.Unmarshal(raw, &recs); err != nil {
			t.Fatalf("decode list: %v: %s", err, raw)
		}
		out := map[string]servedRecord{}
		for _, r := range recs {
			out[r.ID] = r
		}
		if len(out) != 4 {
			t.Fatalf("%s lists %d records, want 4", persona, len(out))
		}
		return out
	}

	owner := list(prOwner)
	if cb := owner[ownDraft].CreatedBy; cb == nil || *cb != f.userID[prOwner] {
		t.Errorf("created_by of the owner's draft = %v, want %s", cb, f.userID[prOwner])
	}
	if cb := owner[legacy].CreatedBy; cb != nil {
		t.Errorf("created_by of the legacy record = %q, want null", *cb)
	}
	checkPerms(t, "owner/own draft", owner[ownDraft].Permissions, perms(true, true, "submitted"))
	checkPerms(t, "owner/own approved", owner[ownApproved].Permissions, perms(false, false))
	checkPerms(t, "owner/peer's submitted", owner[peerSubmitted].Permissions, perms(false, false))
	checkPerms(t, "owner/legacy", owner[legacy].Permissions, perms(false, false))

	peer := list(prPeer)
	checkPerms(t, "peer/owner's draft", peer[ownDraft].Permissions, perms(false, false))
	checkPerms(t, "peer/own submitted", peer[peerSubmitted].Permissions, perms(true, true, "draft"))

	for _, admin := range []string{prBA, prDev} {
		got := list(admin)
		checkPerms(t, admin+"/draft", got[ownDraft].Permissions, perms(true, true, "submitted", "approved", "rejected"))
		checkPerms(t, admin+"/approved", got[ownApproved].Permissions, perms(true, true, "draft", "submitted", "rejected"))
		checkPerms(t, admin+"/legacy", got[legacy].Permissions, perms(true, true, "draft", "approved", "rejected"))
	}

	// The single read serves the same permissions as the list.
	raw := f.expect(http.StatusOK, http.MethodGet, "/api/records/"+peerSubmitted, prPeer, nil)
	var one servedRecord
	_ = json.Unmarshal(raw, &one)
	checkPerms(t, "peer/own submitted (single read)", one.Permissions, perms(true, true, "draft"))
}

// TestFormRecordApprovalRuleFiresOnlyWhenPermitted — a refused approval
// fires nothing; the business admin's fires the form's form_approval rule.
func TestFormRecordApprovalRuleFiresOnlyWhenPermitted(t *testing.T) {
	f := setupRecordFixture(t)
	ctx := context.Background()
	// The rule's workflow does not exist: every fire still leaves an
	// execution row (TriggerRule records a failed one), which is what is
	// counted here.
	var ruleID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, source_form_id)
		VALUES ($1::uuid, 'on approval', 'form_approval', 'no-such-workflow', $2::uuid) RETURNING id::text`,
		f.appID, f.formID).Scan(&ruleID); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	executions := func() int {
		var n int
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM workflow.execution WHERE rule_id=$1::uuid`, ruleID).Scan(&n)
		return n
	}

	rec := f.create(prOwner, "submitted", 10)
	f.expect(http.StatusForbidden, http.MethodPut, "/api/records/"+rec, prOwner, map[string]any{"status": "approved"})
	f.expect(http.StatusForbidden, http.MethodPut, "/api/records/"+rec, prPeer, map[string]any{"status": "approved"})
	f.expect(http.StatusNotFound, http.MethodPut, "/api/records/"+rec, prForeignBA, map[string]any{"status": "approved"})
	time.Sleep(1500 * time.Millisecond)
	if n := executions(); n != 0 {
		t.Fatalf("form_approval rule fired %d time(s) on refused approvals", n)
	}

	f.expect(http.StatusOK, http.MethodPut, "/api/records/"+rec, prBA, map[string]any{"status": "approved"})
	deadline := time.Now().Add(10 * time.Second)
	for executions() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("form_approval rule fired %d time(s) after the business admin's approval, want 1", executions())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestFormRecordPutKeepsWhatItOmits — a PUT that names only a status keeps
// the record's fields, and one that names only fields keeps its status. (A
// PUT without a status used to reset the record to draft, and one without
// data blanked it.)
func TestFormRecordPutKeepsWhatItOmits(t *testing.T) {
	f := setupRecordFixture(t)
	rec := f.create(prOwner, "submitted", 42)
	path := "/api/records/" + rec

	f.expect(http.StatusOK, http.MethodPut, path, prBA, map[string]any{"status": "approved"})
	if status, amount, _ := f.dbRecord(rec); status != "approved" || amount != 42 {
		t.Errorf("after a status-only PUT: %s/%v, want approved/42", status, amount)
	}
	f.expect(http.StatusOK, http.MethodPut, path, prBA, map[string]any{"data": map[string]any{"amount": 43}})
	if status, amount, _ := f.dbRecord(rec); status != "approved" || amount != 43 {
		t.Errorf("after a data-only PUT: %s/%v, want approved/43", status, amount)
	}
	f.expect(http.StatusBadRequest, http.MethodPut, path, prBA, map[string]any{"status": "closed"})
}

// TestFormRecordImportStatusIsTheCreatorsRule — an import creates records
// as its caller, in the statuses they may create.
func TestFormRecordImportStatusIsTheCreatorsRule(t *testing.T) {
	f := setupRecordFixture(t)
	importPath := "/api/forms/" + f.formID + "/import"

	f.expect(http.StatusUnprocessableEntity, http.MethodPost, importPath, prOwner, map[string]string{"csv": "amount,status\n5,submitted\n6,approved\n"})
	var n int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, f.formID).Scan(&n)
	if n != 0 {
		t.Fatalf("a business user's import of an approved row created %d record(s)", n)
	}
	f.expect(http.StatusOK, http.MethodPost, importPath, prOwner, map[string]string{"csv": "amount,status\n5,submitted\n"})
	f.expect(http.StatusOK, http.MethodPost, importPath, prBA, map[string]string{"csv": "amount,status\n6,approved\n"})

	rows, err := f.pool.Query(context.Background(),
		`SELECT status::text, created_by::text FROM runtime.form_record WHERE form_id=$1::uuid ORDER BY status`, f.formID)
	if err != nil {
		t.Fatalf("read imported records: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var st, by string
		_ = rows.Scan(&st, &by)
		got = append(got, st+"/"+by)
	}
	want := []string{"submitted/" + f.userID[prOwner], "approved/" + f.userID[prBA]}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imported records %v, want %v", got, want)
	}
}

// TestFormRecordConditionalWrite — the store writes only while the record
// is in the status the permission check was made against.
func TestFormRecordConditionalWrite(t *testing.T) {
	f := setupRecordFixture(t)
	rec := f.create(prOwner, "submitted", 1)
	store := crudapp.NewStore(f.pool)
	ctx := context.Background()
	if err := store.UpdateRecordFrom(ctx, rec, "draft", "submitted", map[string]any{"amount": 2}); !errors.Is(err, crudapp.ErrRecordChanged) {
		t.Errorf("UpdateRecordFrom from a stale status: err %v, want ErrRecordChanged", err)
	}
	if err := store.DeleteRecordFrom(ctx, rec, "approved"); !errors.Is(err, crudapp.ErrRecordChanged) {
		t.Errorf("DeleteRecordFrom from a stale status: err %v, want ErrRecordChanged", err)
	}
	if status, amount, exists := f.dbRecord(rec); !exists || status != "submitted" || amount != 1 {
		t.Errorf("record after stale writes: exists=%v %s/%v, want submitted/1", exists, status, amount)
	}
	if err := store.UpdateRecordFrom(ctx, rec, "submitted", "approved", map[string]any{"amount": 2}); err != nil {
		t.Errorf("UpdateRecordFrom from the current status: %v", err)
	}
}
