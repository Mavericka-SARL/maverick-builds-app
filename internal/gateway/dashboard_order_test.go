package gateway

// PUT /api/developer/dashboards/order: a developer sets the order of a
// revision's dashboards, and every list of them — the developer's, the
// Dashboards page's tabs (whose first is where people land) — follows it. A
// dashboard never ordered lists after the ordered ones, by name. Only the
// developer role reaches it; the order stays inside the revision and travels
// with revision duplication and model export/import.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestReorderDashboards_Route(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	const dev = "mm-dev"
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	// Start any existing dashboards of the revision out of the way: the test
	// names its own and asks for exactly them.
	if _, err := f.pool.Exec(ctx, `DELETE FROM model.dashboard_def WHERE revision_id=$1::uuid`, f.revA); err != nil {
		t.Fatal(err)
	}
	dash := func(name string) string {
		return q(`INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text`,
			f.modelA, f.revA, name)
	}
	plan, guide, inputs := dash("Plan Summary"), dash("Start here"), dash("Inputs")
	put := func(sub string, ids ...string) (int, string) {
		t.Helper()
		code, raw := f.do(t, "PUT", "/api/developer/dashboards/order", sub, f.app1, f.modelA, map[string]any{"dashboard_ids": ids})
		return code, string(raw)
	}
	listed := func(path, sub string) string {
		t.Helper()
		code, raw := f.do(t, "GET", path, sub, f.app1, f.modelA, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, code, raw)
		}
		var ds []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &ds)
		names := make([]string, len(ds))
		for i, d := range ds {
			names[i] = d.Name
		}
		return strings.Join(names, ",")
	}

	// Never ordered: by name, as before.
	if got := listed("/api/developer/dashboards?revision_id="+f.revA, dev); got != "Inputs,Plan Summary,Start here" {
		t.Fatalf("unordered list: %s", got)
	}
	if code, raw := put(dev, guide, inputs, plan); code != http.StatusOK {
		t.Fatalf("reorder: %d %s", code, raw)
	}
	want := "Start here,Inputs,Plan Summary"
	if got := listed("/api/developer/dashboards?revision_id="+f.revA, dev); got != want {
		t.Errorf("developer list after the reorder: %s, want %s", got, want)
	}
	if got := listed("/api/dashboards?revision_id="+f.revA, dev); got != want {
		t.Errorf("Dashboards page after the reorder: %s, want %s", got, want)
	}
	audited := q(`SELECT metadata->>'order' FROM audit.audit_event WHERE event_type='dashboard.reordered' ORDER BY occurred_at DESC LIMIT 1`)
	if audited != "Start here | Inputs | Plan Summary" {
		t.Errorf("audit order %q", audited)
	}

	// A dashboard added later lists after the ordered ones.
	dash("Approval")
	if got := listed("/api/developer/dashboards?revision_id="+f.revA, dev); got != want+",Approval" {
		t.Errorf("a new dashboard: %s", got)
	}

	// The list must be exactly the revision's dashboards, each once.
	all := strings.Split(q(`SELECT string_agg(id::text, ',' ORDER BY sort_order NULLS LAST, name) FROM model.dashboard_def WHERE revision_id=$1::uuid`, f.revA), ",")
	for name, ids := range map[string][]string{
		"one missing": all[:3],
		"one twice":   {all[0], all[1], all[2], all[2]},
	} {
		if code, raw := put(dev, ids...); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, code, raw)
		}
	}

	// Only the developer role reaches it.
	for _, sub := range []string{"mm-ba1", "mm-user", "mm-tenant-admin"} {
		if code, raw := put(sub, all...); code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", sub, code, raw)
		}
	}
	// Another tenant's dashboard, or none, is a 404.
	theirs := q(`INSERT INTO model.dashboard_def (model_id, name) VALUES ($1::uuid, 'Theirs') RETURNING id::text`, f.modelD)
	for _, id := range []string{theirs, "00000000-0000-0000-0000-000000000000"} {
		if code, raw := put(dev, id); code != http.StatusNotFound {
			t.Errorf("dashboard %s: %d %s, want 404", id, code, raw)
		}
	}

	// ── a duplicated revision carries the order ──────────────────────────
	code, raw := f.do(t, "POST", "/api/developer/revisions", dev, f.app1, f.modelA, map[string]any{"name": "Copy", "source_revision_id": f.revA})
	if code != http.StatusOK {
		t.Fatalf("duplicate the revision: %d %s", code, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &created)
	if got := listed("/api/developer/dashboards?revision_id="+created.ID, dev); got != want+",Approval" {
		t.Errorf("duplicated revision's order %s", got)
	}

	// ── model export and import carry it ─────────────────────────────────
	code, raw = f.do(t, "GET", "/api/admin/models/"+f.modelA+"/export?revision_id="+f.revA, "mm-tenant-admin", f.app1, "", nil)
	if code != http.StatusOK {
		t.Fatalf("export: %d %.300s", code, raw)
	}
	code, raw = f.do(t, "POST", "/api/admin/models/import", "mm-tenant-admin", f.app2, "",
		map[string]any{"application_id": f.app2, "model_name": "Imported", "package": json.RawMessage(raw)})
	if code != http.StatusOK {
		t.Fatalf("import: %d %s", code, raw)
	}
	var imported struct {
		RevisionID string `json:"revision_id"`
	}
	_ = json.Unmarshal(raw, &imported)
	if got := q(`SELECT string_agg(name, ',' ORDER BY sort_order NULLS LAST, name) FROM model.dashboard_def WHERE revision_id=$1::uuid`, imported.RevisionID); got != want+",Approval" {
		t.Errorf("imported order %s", got)
	}
}
