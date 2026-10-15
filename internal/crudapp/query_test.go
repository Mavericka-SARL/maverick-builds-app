package crudapp_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// TestRecordQuery covers the records query against a real database: what a
// person may see, filters, search, sort, pages and totals, auto-codes, and
// records without a status.
func TestRecordQuery(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)
	alice, bob := insertUser(t, store), insertUser(t, store)
	if _, err := store.Pool().Exec(ctx, `UPDATE identity.user SET display_name='Alice' WHERE id=$1::uuid`, alice); err != nil {
		t.Fatal(err)
	}
	dimID := "6b0f2f4e-0000-4000-8000-000000000001"
	if _, err := store.Pool().Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label) VALUES
		($1::uuid, 'EU', 'Europe'), ($1::uuid, 'US', 'United States'), ($1::uuid, 'APAC', 'Asia Pacific')`, dimID); err != nil {
		t.Fatal(err)
	}

	fields := []crudapp.FormField{
		{Name: "code", Label: "Code", Type: crudapp.FieldAutoCode, CodePrefix: "EXP-", CodeDigits: 4},
		{Name: "title", Label: "Title", Type: "text"},
		{Name: "amount", Label: "Amount", Type: "number"},
		{Name: "spent_on", Label: "Spent on", Type: "date"},
		{Name: "region", Label: "Region", Type: "dimension", DimensionID: dimID},
		{Name: "metric", Label: "Metric", Type: "metric"},
		{Name: "budget", Label: "Budget", Type: "metric", MetricID: "6b0f2f4e-0000-4000-8000-0000000000ff"},
	}
	form, err := store.CreateForm(ctx, modelID, "", "expenses", "Expenses", fields)
	if err != nil {
		t.Fatal(err)
	}
	if form.HasStatus() {
		t.Fatal("a table without a status field reports a status")
	}

	add := func(user string, data map[string]any) *crudapp.FormRecord {
		t.Helper()
		data["code"] = "CHOSEN-BY-CALLER" // the server gives the code
		rec, err := store.CreateRecord(ctx, form.ID, user, data)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	r1 := add(alice, map[string]any{"title": "Train to Paris", "amount": 120.0, "spent_on": "2026-01-15", "region": "EU", "metric": "travel"})
	add(alice, map[string]any{"title": "Hotel Berlin", "amount": 300.0, "spent_on": "2026-02-03", "region": "EU", "metric": "travel"})
	add(bob, map[string]any{"title": "Taxi NYC", "amount": "45.5", "spent_on": "2026-02-20", "region": "US", "metric": "travel"})
	add(bob, map[string]any{"title": "Server", "amount": 1000.0, "spent_on": "2026-03-01", "region": "APAC", "metric": "hardware"})
	add(bob, map[string]any{"title": "Budget note", "budget": 5.0, "region": "US"})

	t.Run("records get the next code and no status", func(t *testing.T) {
		if r1.Data["code"] != "EXP-0001" {
			t.Errorf("first record's code = %v, want EXP-0001", r1.Data["code"])
		}
		if r1.Status != "" {
			t.Errorf("a record of a table without a status field has status %q", r1.Status)
		}
		got, err := store.GetRecord(ctx, r1.ID)
		if err != nil || got.Status != "" {
			t.Fatalf("read back: %v, status %q", err, got.Status)
		}
	})

	t.Run("a deleted record's code is not given again", func(t *testing.T) {
		last := add(alice, map[string]any{"title": "Mistake"})
		if last.Data["code"] != "EXP-0006" {
			t.Fatalf("sixth record's code = %v", last.Data["code"])
		}
		if err := store.DeleteRecord(ctx, last.ID); err != nil {
			t.Fatal(err)
		}
		next := add(alice, map[string]any{"title": "Another mistake"})
		if next.Data["code"] != "EXP-0007" {
			t.Errorf("after deleting EXP-0006 the next code = %v, want EXP-0007", next.Data["code"])
		}
		_ = store.DeleteRecord(ctx, next.ID)
	})

	all := crudapp.Visibility{}
	list := func(t *testing.T, vis crudapp.Visibility, q crudapp.Query) []string {
		t.Helper()
		res, err := store.QueryRecords(ctx, form, vis, alice, q)
		if err != nil {
			t.Fatalf("query %+v: %v", q, err)
		}
		titles := make([]string, len(res.Records))
		for i, r := range res.Records {
			titles[i], _ = r.Data["title"].(string)
		}
		if res.Total < len(titles) {
			t.Errorf("total %d below the %d records served", res.Total, len(titles))
		}
		return titles
	}
	eq := func(t *testing.T, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	t.Run("filters", func(t *testing.T) {
		eq(t, list(t, all, crudapp.Query{Filters: []crudapp.Filter{{Field: "region", Op: "eq", Value: "eu"}}, Sort: []crudapp.Sort{{Field: "amount"}}}),
			"Train to Paris", "Hotel Berlin")
		eq(t, list(t, all, crudapp.Query{Filters: []crudapp.Filter{{Field: "amount", Op: "gt", Value: 100}}, Sort: []crudapp.Sort{{Field: "amount", Desc: true}}}),
			"Server", "Hotel Berlin", "Train to Paris")
		eq(t, list(t, all, crudapp.Query{Filters: []crudapp.Filter{{Field: "spent_on", Op: "between", Values: []any{"2026-02-01", "2026-02-28"}}}, Sort: []crudapp.Sort{{Field: "spent_on"}}}),
			"Hotel Berlin", "Taxi NYC")
		eq(t, list(t, all, crudapp.Query{Filters: []crudapp.Filter{{Field: "region", Op: "in", Values: []any{"US", "APAC"}}, {Field: "amount", Op: "not_empty"}}, Sort: []crudapp.Sort{{Field: "title"}}}),
			"Server", "Taxi NYC")
		eq(t, list(t, all, crudapp.Query{Filters: []crudapp.Filter{{Field: "created_by", Op: "eq", Value: "me"}}, Sort: []crudapp.Sort{{Field: "code"}}}),
			"Train to Paris", "Hotel Berlin")
		eq(t, list(t, all, crudapp.Query{Filters: []crudapp.Filter{{Field: "code", Op: "eq", Value: "EXP-0004"}}}), "Server")
		if _, err := store.QueryRecords(ctx, form, all, alice, crudapp.Query{Filters: []crudapp.Filter{{Field: "nope", Op: "eq", Value: "x"}}}); !crudapp.IsQueryError(err) {
			t.Errorf("a filter on a field the table lacks: %v, want a query error", err)
		}
		if _, err := store.QueryRecords(ctx, form, all, alice, crudapp.Query{Filters: []crudapp.Filter{{Field: "title", Op: "gt", Value: "x"}}}); !crudapp.IsQueryError(err) {
			t.Errorf("gt on text: %v, want a query error", err)
		}
	})

	t.Run("search reads values and member names", func(t *testing.T) {
		eq(t, list(t, all, crudapp.Query{Search: "berlin"}), "Hotel Berlin")
		eq(t, list(t, all, crudapp.Query{Search: "asia"}), "Server")
		eq(t, list(t, all, crudapp.Query{Search: "exp-0002"}), "Hotel Berlin")
		eq(t, list(t, all, crudapp.Query{Search: "%"}))
	})

	t.Run("pages", func(t *testing.T) {
		res, err := store.QueryRecords(ctx, form, all, alice, crudapp.Query{Sort: []crudapp.Sort{{Field: "code"}}, Limit: 2, Offset: 2})
		if err != nil {
			t.Fatal(err)
		}
		if res.Total != 5 || len(res.Records) != 2 || res.Records[0].Data["code"] != "EXP-0003" {
			t.Errorf("page 2 of 2: total %d, %d records, first %v", res.Total, len(res.Records), res.Records[0].Data["code"])
		}
	})

	t.Run("what a person may see", func(t *testing.T) {
		eq(t, list(t, crudapp.Visibility{OwnerID: bob}, crudapp.Query{Sort: []crudapp.Sort{{Field: "code"}}}), "Taxi NYC", "Server", "Budget note")
		eq(t, list(t, crudapp.Visibility{HiddenCodes: map[string][]string{"region": {"EU", "APAC"}}}, crudapp.Query{Sort: []crudapp.Sort{{Field: "code"}}}), "Taxi NYC", "Budget note")
		eq(t, list(t, crudapp.Visibility{HiddenNames: map[string][]string{"metric": {"hardware"}}}, crudapp.Query{Filters: []crudapp.Filter{{Field: "region", Op: "eq", Value: "APAC"}}}))
		eq(t, list(t, crudapp.Visibility{WithheldFields: []string{"budget"}}, crudapp.Query{Filters: []crudapp.Filter{{Field: "region", Op: "eq", Value: "US"}}}), "Taxi NYC")
		eq(t, list(t, crudapp.Visibility{Nothing: true}, crudapp.Query{}))
		if _, err := store.VisibleRecord(ctx, form, crudapp.Visibility{OwnerID: bob}, r1.ID); err == nil {
			t.Error("Bob reads Alice's record in a table showing each person their own")
		}
		if _, err := store.VisibleRecord(ctx, form, all, r1.ID); err != nil {
			t.Errorf("reading a visible record: %v", err)
		}
	})

	t.Run("totals", func(t *testing.T) {
		res, err := store.QueryTotals(ctx, form, crudapp.Visibility{HiddenCodes: map[string][]string{"region": {"APAC"}}}, alice, crudapp.Query{
			GroupBy:    []crudapp.Group{{Field: "region"}},
			Aggregates: []crudapp.Aggregate{{Op: "count"}, {Op: "sum", Field: "amount"}, {Op: "max", Field: "spent_on"}},
			Sort:       []crudapp.Sort{{Field: "sum_amount", Desc: true}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.TotalGroups != 2 || len(res.Rows) != 2 {
			t.Fatalf("groups: %+v", res)
		}
		eu := res.Rows[0]
		if eu["region"] != "EU" || eu["region_label"] != "Europe" || eu["count"] != int64(2) || eu["sum_amount"] != 420.0 || eu["max_spent_on"] != "2026-02-03" {
			t.Errorf("EU row = %v", eu)
		}
		us := res.Rows[1]
		if us["region"] != "US" || us["sum_amount"] != 45.5 || us["count"] != int64(2) {
			t.Errorf("US row (a numeric string adds up; the budget note has no amount) = %v", us)
		}

		byMonth, err := store.QueryTotals(ctx, form, all, alice, crudapp.Query{
			GroupBy:    []crudapp.Group{{Field: "spent_on", Period: "month"}, {Field: "created_by"}},
			Aggregates: []crudapp.Aggregate{{Op: "avg", Field: "amount"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		first := byMonth.Rows[0]
		if first["spent_on_month"] != "2026-01" || first["created_by_label"] != "Alice" || first["avg_amount"] != 120.0 {
			t.Errorf("first month row = %v", first)
		}

		one, err := store.QueryTotals(ctx, form, all, alice, crudapp.Query{Filters: []crudapp.Filter{{Field: "metric", Op: "eq", Value: "travel"}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(one.Rows) != 1 || one.Rows[0]["count"] != int64(3) {
			t.Errorf("count of travel records = %v", one.Rows)
		}
		if _, err := store.QueryTotals(ctx, form, all, alice, crudapp.Query{Aggregates: []crudapp.Aggregate{{Op: "sum", Field: "title"}}}); !crudapp.IsQueryError(err) {
			t.Errorf("sum of text: %v, want a query error", err)
		}
	})

	t.Run("a new auto-code field numbers the records already there", func(t *testing.T) {
		withRef := append(append([]crudapp.FormField{}, fields...), crudapp.FormField{Name: "ref", Label: "Ref", Type: crudapp.FieldAutoCode, CodePrefix: "R"})
		change, err := store.UpdateForm(ctx, form.ID, form.Name, form.Label, withRef)
		if err != nil {
			t.Fatal(err)
		}
		if change.CodesAssigned != 5 || change.StatusChanged {
			t.Errorf("change = %+v, want 5 codes and no status change", change)
		}
		got, _ := store.GetRecord(ctx, r1.ID)
		if got.Data["ref"] != "R0001" {
			t.Errorf("oldest record's new code = %v, want R0001", got.Data["ref"])
		}
		withStatus := append(withRef, crudapp.FormField{Name: "status", Label: "Status", Type: crudapp.FieldStatus})
		if change, err = store.UpdateForm(ctx, form.ID, form.Name, form.Label, withStatus); err != nil || !change.StatusChanged {
			t.Errorf("adding a status field: %+v, %v", change, err)
		}
		rec, err := store.CreateRecord(ctx, form.ID, alice, map[string]any{"title": "Now with status", "status": "approved"})
		if err != nil {
			t.Fatal(err)
		}
		if rec.Status != crudapp.StatusDraft || rec.Data["status"] != nil {
			t.Errorf("a new record of a table with a status field: status %q, data %v", rec.Status, rec.Data)
		}
	})

	t.Run("fields are validated", func(t *testing.T) {
		for name, fs := range map[string][]crudapp.FormField{
			"no name":         {{Name: "", Type: "text"}},
			"same name":       {{Name: "a", Type: "text"}, {Name: "a", Type: "number"}},
			"unknown type":    {{Name: "a", Type: "colour"}},
			"two statuses":    {{Name: "a", Type: "status"}, {Name: "b", Type: "status"}},
			"too many digits": {{Name: "a", Type: "autocode", CodeDigits: 13}},
		} {
			if crudapp.ValidateFields(fs) == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		if err := crudapp.ValidateFields(fields); err != nil {
			t.Errorf("valid fields refused: %v", err)
		}
	})
}
