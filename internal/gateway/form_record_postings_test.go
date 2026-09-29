package gateway

// What form records post into metrics follows who may change them: a form
// sync is an administrator's, a refused re-post leaves the record's
// contribution as it was, and a record that stops contributing — deleted,
// or its value removed — takes its value back out of the metric. Also the
// builder routes on the form itself (PATCH/DELETE /api/forms/{id}), whose
// delete cascades to every record, and a tenant-level application, whose
// records any business_admin of its tenant administers.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// mapRecordsToMetric has the developer create an input metric and a live
// mapping of the form's amount field into it, posting on statuses.
func (f *recordFixture) mapRecordsToMetric(statuses ...string) (mappingID, metricID string) {
	f.t.Helper()
	raw := f.expect(http.StatusOK, http.MethodPost, "/api/developer/metrics", prDev, map[string]any{
		"name": "expense_total", "is_input": true, "agg_rule": "sum", "format": "number", "revision_id": f.revID,
	})
	metricID, _ = decodeObj(f.t, raw)["id"].(string)
	raw = f.expect(http.StatusOK, http.MethodPost, "/api/developer/form-integrations?revision_id="+f.revID, prDev, map[string]any{
		"form_id": f.formID, "name": "expenses", "source_field": "amount", "target_metric_id": metricID,
		"aggregation": "sum", "posting_statuses": statuses, "live_posting": true,
	})
	mappingID, _ = decodeObj(f.t, raw)["id"].(string)
	if metricID == "" || mappingID == "" {
		f.t.Fatalf("mapping setup: metric %q, mapping %q", metricID, mappingID)
	}
	return mappingID, metricID
}

// posted is the mapping's posting rows and the total it holds in the metric.
func (f *recordFixture) posted(mappingID, metricID string) (postings int, total float64) {
	f.t.Helper()
	ctx := context.Background()
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record_posting WHERE mapping_id=$1::uuid`, mappingID).Scan(&postings); err != nil {
		f.t.Fatalf("count postings: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(SUM(value), 0) FROM runtime.fact_input WHERE source_ref=$1::uuid AND metric_id=$2::uuid`, mappingID, metricID).Scan(&total); err != nil {
		f.t.Fatalf("sum fact_input: %v", err)
	}
	return postings, total
}

// waitPosted waits for the detached posting work to settle on want postings
// totalling wantTotal, and fails with what it last saw.
func (f *recordFixture) waitPosted(label, mappingID, metricID string, wantPostings int, wantTotal float64) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, total := f.posted(mappingID, metricID)
		if n == wantPostings && total == wantTotal {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("%s: %d posting(s) totalling %v in the metric, want %d totalling %v", label, n, total, wantPostings, wantTotal)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestFormSyncIsAnAdministrators — a sync re-posts every record of the
// form, other people's included, so a business user who may only read them
// is refused; the business admin and the developer sync.
func TestFormSyncIsAnAdministrators(t *testing.T) {
	f := setupRecordFixture(t)
	mappingID, metricID := f.mapRecordsToMetric("approved")
	f.create(prBA, "approved", 10)
	f.create(prBA, "approved", 20)
	f.waitPosted("two approved records", mappingID, metricID, 2, 30)

	syncPath := "/api/forms/" + f.formID + "/sync"
	for _, p := range []string{prOwner, prPeer} {
		f.expect(http.StatusForbidden, http.MethodPost, syncPath, p, nil)
	}
	if n, total := f.posted(mappingID, metricID); n != 2 || total != 30 {
		t.Errorf("after the refused syncs: %d posting(s) totalling %v, want 2 totalling 30", n, total)
	}
	for _, p := range []string{prBA, prDev} {
		raw := f.expect(http.StatusOK, http.MethodPost, syncPath, p, nil)
		if got := decodeObj(t, raw)["records_processed"]; got != float64(2) {
			t.Errorf("%s sync processed %v record(s), want 2", p, got)
		}
	}
	f.waitPosted("after the administrators' syncs", mappingID, metricID, 2, 30)
}

// TestFormRecordRefusedRepostKeepsContribution — when the write guard
// refuses a record's re-post (here the creator was made read-only on the
// metric after submitting), the posting the record already holds stays:
// it used to be deleted before the check, so the refused caller withdrew
// the contribution and the next recompute dropped it from the metric.
func TestFormRecordRefusedRepostKeepsContribution(t *testing.T) {
	f := setupRecordFixture(t)
	mappingID, metricID := f.mapRecordsToMetric("submitted")
	rec := f.create(prOwner, "submitted", 10)
	f.waitPosted("the owner's submission", mappingID, metricID, 1, 10)

	f.expect(http.StatusOK, http.MethodPut, "/api/business-admin/users/"+f.userID[prOwner]+"/access-rules", prBA,
		map[string]any{"rules": []map[string]string{{"rule_type": "metric", "ref_id": metricID, "access": "read"}}})
	// The record is the owner's to edit; the metric is no longer theirs to write.
	f.expect(http.StatusOK, http.MethodPut, "/api/records/"+rec, prOwner, map[string]any{"data": map[string]any{"amount": 15}})

	// The re-post runs detached from the request; give it time to act.
	time.Sleep(1500 * time.Millisecond)
	var kept float64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(posted_value), -1) FROM runtime.form_record_posting WHERE mapping_id=$1::uuid AND form_record_id=$2::uuid`,
		mappingID, rec).Scan(&kept); err != nil {
		t.Fatalf("read posting: %v", err)
	}
	if kept != 10 {
		t.Errorf("the record's posting after a refused re-post = %v, want 10 kept (-1: withdrawn)", kept)
	}
	// Another posting recomputes the metric from the posting rows.
	f.create(prBA, "submitted", 5)
	f.waitPosted("after the next submission", mappingID, metricID, 2, 15)
}

// TestFormRecordRetractionTakesValueOut — a record that stops contributing
// takes its value out of the metric: one whose value is removed, and one
// that is deleted (by its creator while submitted, by an administrator once
// approved). The posting rows went, but the metric kept the value until
// some other posting recomputed it.
func TestFormRecordRetractionTakesValueOut(t *testing.T) {
	f := setupRecordFixture(t)
	mappingID, metricID := f.mapRecordsToMetric("submitted", "approved")

	emptied := f.create(prOwner, "submitted", 7)
	f.waitPosted("the owner's submission", mappingID, metricID, 1, 7)
	f.expect(http.StatusOK, http.MethodPut, "/api/records/"+emptied, prOwner, map[string]any{"data": map[string]any{}})
	f.waitPosted("after the owner removed the value", mappingID, metricID, 0, 0)

	own := f.create(prOwner, "submitted", 10)
	f.waitPosted("the owner's second submission", mappingID, metricID, 1, 10)
	f.expect(http.StatusOK, http.MethodDelete, "/api/records/"+own, prOwner, nil)
	f.waitPosted("after the owner deleted it", mappingID, metricID, 0, 0)

	approved := f.create(prBA, "approved", 20)
	kept := f.create(prBA, "approved", 3)
	f.waitPosted("two approved records", mappingID, metricID, 2, 23)
	f.expect(http.StatusOK, http.MethodDelete, "/api/records/"+approved, prBA, nil)
	f.waitPosted("after the admin deleted one", mappingID, metricID, 1, 3)
	if _, _, exists := f.dbRecord(kept); !exists {
		t.Errorf("the other approved record went with the delete")
	}
}

// TestFormBuilderRoutesStayInScope — PATCH and DELETE /api/forms/{id} are
// developer routes, and dev() checks only that the caller is a developer
// somewhere: a developer of another tenant renamed and deleted this
// tenant's form, and the delete took every record with it.
func TestFormBuilderRoutesStayInScope(t *testing.T) {
	f := setupRecordFixture(t)
	f.create(prOwner, "submitted", 10)
	f.create(prBA, "approved", 20)
	formPath := "/api/forms/" + f.formID
	fields := []map[string]any{{"name": "amount", "label": "Amount", "type": "number"}}

	// requireResourceAccess: 403 for a form that exists outside the scope.
	f.expect(http.StatusForbidden, http.MethodPatch, formPath, prForeignDev, map[string]any{"name": "taken", "label": "Taken", "fields": fields})
	f.expect(http.StatusForbidden, http.MethodDelete, formPath, prForeignDev, nil)

	ctx := context.Background()
	var name string
	var records int
	if err := f.pool.QueryRow(ctx, `SELECT name FROM model.form_def WHERE id=$1::uuid`, f.formID).Scan(&name); err != nil {
		t.Fatalf("the form is gone after a foreign developer's delete: %v", err)
	}
	if name != "expense" {
		t.Errorf("form name = %q after a foreign developer's rename, want expense", name)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, f.formID).Scan(&records); err != nil || records != 2 {
		t.Errorf("records = %d (err %v) after a foreign developer's delete, want 2", records, err)
	}

	// The tenant's own developer still changes it.
	f.expect(http.StatusOK, http.MethodPatch, formPath, prDev, map[string]any{"name": "expense", "label": "Expenses", "fields": fields})
}

// TestFormRecordTenantLevelApplication — a tenant-level application
// (workspace_id NULL) belongs to every workspace of its tenant, so a
// business_admin of any of them administers its records (the arm of
// resolveFormRecordScope that mirrors roleReachesAppSQL's business arms),
// while a business_user of the tenant only reaches them and another
// tenant's business_admin does not.
func TestFormRecordTenantLevelApplication(t *testing.T) {
	f := setupRecordFixture(t)
	ctx := context.Background()
	var shared string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES (NULL, $1::uuid, 'Shared', 'planning') RETURNING id::text`,
		f.custA).Scan(&shared); err != nil {
		t.Fatalf("create tenant-level application: %v", err)
	}
	var model, rev string
	if err := f.pool.QueryRow(ctx, `INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Shared') RETURNING id::text`, shared).Scan(&model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, model).Scan(&rev); err != nil {
		t.Fatalf("create revision: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, rev, model); err != nil {
		t.Fatalf("activate revision: %v", err)
	}

	in := func(want int, method, path, persona string, body any) []byte {
		t.Helper()
		status, raw := f.doIn(shared, method, path, persona, body)
		if status != want {
			t.Errorf("%s %s as %s: status %d, want %d: %s", method, path, persona, status, want, raw)
		}
		return raw
	}
	raw := in(http.StatusOK, http.MethodPost, "/api/forms", prDev, map[string]any{
		"name": "shared_expense", "label": "Shared expense",
		"fields": []map[string]any{{"name": "amount", "label": "Amount", "type": "number"}},
	})
	formID, _ := decodeObj(t, raw)["id"].(string)
	if formID == "" {
		t.Fatalf("create form in the tenant-level application: %s", raw)
	}
	raw = in(http.StatusOK, http.MethodPost, "/api/forms/"+formID+"/records", prOwner,
		map[string]any{"data": map[string]any{"amount": 10}, "status": "submitted"})
	rec, _ := decodeObj(t, raw)["id"].(string)
	recPath := "/api/records/" + rec

	// Both business admins of the tenant administer it — the one of the
	// owner's workspace and the one of the other workspace.
	for _, ba := range []string{prBA, prBAOtherWS} {
		var got servedRecord
		_ = json.Unmarshal(in(http.StatusOK, http.MethodGet, recPath, ba, nil), &got)
		checkPerms(t, ba+"/tenant-level record", got.Permissions, perms(true, true, "draft", "approved", "rejected"))
	}
	in(http.StatusOK, http.MethodPut, recPath, prBAOtherWS, map[string]any{"status": "approved"})
	if status, _, _ := f.dbRecord(rec); status != "approved" {
		t.Errorf("status after the other workspace's business admin approved = %q, want approved", status)
	}

	// A business user of the tenant reaches it but does not decide it.
	var peer servedRecord
	_ = json.Unmarshal(in(http.StatusOK, http.MethodGet, recPath, prPeer, nil), &peer)
	checkPerms(t, "peer/tenant-level record", peer.Permissions, perms(false, false))
	in(http.StatusForbidden, http.MethodPut, recPath, prPeer, map[string]any{"status": "rejected"})

	// Another tenant's business admin does not reach it at all.
	in(http.StatusNotFound, http.MethodGet, recPath, prForeignBA, nil)
	in(http.StatusNotFound, http.MethodPut, recPath, prForeignBA, map[string]any{"status": "rejected"})
	in(http.StatusNotFound, http.MethodDelete, recPath, prForeignBA, nil)
	if status, _, exists := f.dbRecord(rec); !exists || status != "approved" {
		t.Errorf("record after the refused calls: exists=%v status=%q, want approved", exists, status)
	}
}
