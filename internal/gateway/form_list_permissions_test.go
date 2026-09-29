package gateway

// GET /api/forms tells each caller what they may do with a form's records
// as a whole — sync them into the metrics, and the statuses a new record
// may take — from the scope the record routes decide by. Without it the
// interface offered "Sync to grid" to people POST /api/forms/{id}/sync
// refuses. And a PUT that re-sends a record's current status changes no
// status, whoever sends it.

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"
)

type servedFormPerms struct {
	ID          string `json:"id"`
	Permissions *struct {
		Sync           bool     `json:"sync"`
		CreateStatuses []string `json:"create_statuses"`
	} `json:"permissions"`
}

// TestFormListCarriesCallerPermissions — every form GET /api/forms lists
// carries sync and create_statuses for the caller, and they are exactly
// what POST /api/forms/{id}/sync and POST /api/forms/{id}/records accept.
// The model has two forms: the scope is resolved once per model, and the
// second form must carry the same permissions as the first.
func TestFormListCarriesCallerPermissions(t *testing.T) {
	f := setupRecordFixture(t)
	status, body := f.do(http.MethodPost, "/api/forms", prDev, map[string]any{
		"name": "expense_two", "label": "Expense two",
		"fields": []map[string]any{{"name": "amount", "label": "Amount", "type": "number"}},
	})
	if status != http.StatusOK {
		t.Fatalf("create second form as developer: %d %s", status, body)
	}
	formIDs := []string{f.formID, decodeObj(t, body)["id"].(string)}
	all := []string{"draft", "submitted", "approved", "rejected"}
	cases := []struct {
		persona string
		sync    bool
		create  []string
	}{
		{prOwner, false, []string{"draft", "submitted"}},
		{prPeer, false, []string{"draft", "submitted"}},
		{prBA, true, all},
		{prDev, true, all},
		{prTA, true, all},
	}
	for _, c := range cases {
		raw := f.expect(http.StatusOK, http.MethodGet, "/api/forms", c.persona, nil)
		var forms []servedFormPerms
		if err := json.Unmarshal(raw, &forms); err != nil {
			t.Fatalf("%s: decode forms %s: %v", c.persona, raw, err)
		}
		for _, formID := range formIDs {
			i := slices.IndexFunc(forms, func(s servedFormPerms) bool { return s.ID == formID })
			if i < 0 {
				t.Fatalf("%s: form %s not listed: %s", c.persona, formID, raw)
			}
			p := forms[i].Permissions
			if p == nil {
				t.Errorf("%s: form %s listed without permissions: %s", c.persona, formID, raw)
				continue
			}
			if p.Sync != c.sync || !reflect.DeepEqual(p.CreateStatuses, c.create) {
				t.Errorf("%s: form %s permissions sync=%v create_statuses=%v, want sync=%v create_statuses=%v",
					c.persona, formID, p.Sync, p.CreateStatuses, c.sync, c.create)
			}

			// What the list says is what the server accepts.
			wantSync := http.StatusForbidden
			if p.Sync {
				wantSync = http.StatusOK
			}
			f.expect(wantSync, http.MethodPost, "/api/forms/"+formID+"/sync", c.persona, nil)
			for _, st := range all {
				want := http.StatusForbidden
				if slices.Contains(p.CreateStatuses, st) {
					want = http.StatusOK
				}
				f.expect(want, http.MethodPost, "/api/forms/"+formID+"/records", c.persona,
					map[string]any{"data": map[string]any{"amount": 1}, "status": st})
			}
		}
	}
}

// TestFormRecordResendingCurrentStatusIsNoChange — a PUT whose status is the
// record's current one is a change of fields alone, for every caller: the
// creator edits the fields of their own draft and submitted records, an
// administrator those of a decided or legacy record, and the audit trail
// records no status change. Whoever may not edit the record is refused as
// such, not as a status move.
func TestFormRecordResendingCurrentStatusIsNoChange(t *testing.T) {
	f := setupRecordFixture(t)
	put := func(want int, persona, rec, status string, amount float64) []byte {
		t.Helper()
		return f.expect(want, http.MethodPut, "/api/records/"+rec, persona,
			map[string]any{"data": map[string]any{"amount": amount}, "status": status})
	}
	check := func(rec, wantStatus string, wantAmount float64) {
		t.Helper()
		if status, amount, _ := f.dbRecord(rec); status != wantStatus || amount != wantAmount {
			t.Errorf("record %s: %s/%v, want %s/%v", rec, status, amount, wantStatus, wantAmount)
		}
	}

	// The creator, on their own draft and submitted records.
	draft := f.create(prOwner, "draft", 1)
	put(http.StatusOK, prOwner, draft, "draft", 2)
	check(draft, "draft", 2)
	submitted := f.create(prOwner, "submitted", 1)
	put(http.StatusOK, prOwner, submitted, "submitted", 3)
	check(submitted, "submitted", 3)

	// An administrator, on a decided record and on a legacy one.
	approved := f.create(prBA, "approved", 1)
	for i, admin := range []string{prBA, prDev, prTA} {
		put(http.StatusOK, admin, approved, "approved", float64(10+i))
		check(approved, "approved", float64(10+i))
	}
	var legacy string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO runtime.form_record (form_id, data, status, created_by) VALUES ($1::uuid, '{"amount":4}', 'rejected', NULL) RETURNING id::text`,
		f.formID).Scan(&legacy); err != nil {
		t.Fatalf("insert legacy record: %v", err)
	}
	put(http.StatusOK, prBA, legacy, "rejected", 5)
	check(legacy, "rejected", 5)

	// None of those saves changed a status.
	var moved int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit.audit_event
		WHERE event_type='form_record.updated' AND resource_id = ANY($1::text[]) AND metadata ? 'previous_status'`,
		[]string{draft, submitted, approved, legacy}).Scan(&moved); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if moved != 0 {
		t.Errorf("%d form_record.updated audit events record a status change for saves that kept the status", moved)
	}

	// Re-sending the status does not open a record to whoever may not edit
	// it: the creator of a decided record, another business user.
	put(http.StatusForbidden, prOwner, approved, "approved", 99)
	for _, rec := range []string{draft, submitted} {
		raw := put(http.StatusForbidden, prPeer, rec, "", 99)
		raw2 := put(http.StatusForbidden, prPeer, rec, map[string]string{draft: "draft", submitted: "submitted"}[rec], 99)
		if string(raw) != string(raw2) {
			t.Errorf("peer re-sending the current status is refused differently from a field edit: %s vs %s", raw2, raw)
		}
	}
	check(approved, "approved", 12)
	check(draft, "draft", 2)
	check(submitted, "submitted", 3)
}
