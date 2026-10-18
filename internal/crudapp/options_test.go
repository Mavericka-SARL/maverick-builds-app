package crudapp

import (
	"strings"
	"testing"
)

func TestFieldOptionsAreChecked(t *testing.T) {
	base := func(extra ...FormField) []FormField {
		return append([]FormField{
			{Name: "code", Label: "Code", Type: FieldAutoCode},
			{Name: "title", Label: "Title", Type: "text", Required: true},
			{Name: "kind", Label: "Kind", Type: "select", Options: []string{"A", "B"}},
			{Name: "due", Label: "Due", Type: "date"},
			{Name: "amount", Label: "Amount", Type: "number"},
			{Name: "region", Label: "Region", Type: "dimension", DimensionID: "d1"},
		}, extra...)
	}
	ok := [][]FormField{
		base(FormField{Name: "site", Label: "Site", Type: FieldURL}, FormField{Name: "notes", Label: "Notes", Type: FieldLongText}),
		base(FormField{Name: "when", Label: "When", Type: "date", Default: "today", Deadline: true, DoneField: "kind", DoneValues: []string{"B"}}),
		base(FormField{Name: "pick", Label: "Pick", Type: "select", Options: []string{"x", "y"}, Default: "y", RequiredWith: "title"}),
		base(FormField{Name: "money", Label: "Money", Type: "number", Format: "money", Default: 1.5, History: true, Core: true}),
		base(FormField{Name: "link", Label: "Link", Type: "dimension", DimensionID: "d2", OnDelete: OnDeleteCascade}),
		base(FormField{Name: "secret", Label: "Secret", Type: "number", Hidden: true, Required: true, Default: 1}),
	}
	for i, fields := range ok {
		if err := ValidateFields(fields); err != nil {
			t.Errorf("valid fields %d refused: %v", i, err)
		}
	}
	bad := map[string]FormField{
		"default on autocode":            {Name: "c2", Type: FieldAutoCode, Default: "X"},
		"select default not a choice":    {Name: "p", Type: "select", Options: []string{"x"}, Default: "z"},
		"date default not a date":        {Name: "w", Type: "date", Default: "tomorrow"},
		"required_with unknown":          {Name: "a", Type: "text", RequiredWith: "nope"},
		"required_with itself":           {Name: "a", Type: "text", RequiredWith: "a"},
		"on_delete on a text field":      {Name: "a", Type: "text", OnDelete: OnDeleteBlock},
		"clear on a required field":      {Name: "a", Type: "dimension", DimensionID: "d", Required: true, OnDelete: OnDeleteClear},
		"unknown on_delete":              {Name: "a", Type: "dimension", DimensionID: "d", OnDelete: "drop"},
		"money on a date":                {Name: "a", Type: "date", Format: "money"},
		"deadline on a number":           {Name: "a", Type: "number", Deadline: true},
		"done_field without deadline":    {Name: "a", Type: "date", DoneField: "kind"},
		"hidden required no default":     {Name: "a", Type: "text", Hidden: true, Required: true},
		"history on status":              {Name: "s", Type: FieldStatus, History: true},
		"unknown type":                   {Name: "a", Type: "currency"},
		"done_values without done_field": {Name: "a", Type: "date", Deadline: true, DoneValues: []string{"x"}},
	}
	for name, f := range bad {
		if err := ValidateFields(base(f)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":               "https://example.com",
		" https://a.example/x?y=1 ": "https://a.example/x?y=1",
		"http://localhost:8080/a":   "http://localhost:8080/a",
		"mailto:anna@example.com":   "mailto:anna@example.com",
	} {
		if got, ok := NormalizeURL(in); !ok || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "not a link", "ftp://x.example", "nodot", "mailto:nobody"} {
		if got, ok := NormalizeURL(in); ok {
			t.Errorf("NormalizeURL(%q) = %q, accepted", in, got)
		}
	}
}

func TestDefaultsAndHiddenFieldsKeepTheirValues(t *testing.T) {
	form := &FormDef{Fields: []FormField{
		{Name: "code", Type: FieldAutoCode},
		{Name: "country", Type: "text", Default: "Russia"},
		{Name: "opened", Type: "date", Default: DefaultToday},
		{Name: "one", Type: "number", Hidden: true, Default: 1.0},
		{Name: "state", Type: FieldStatus},
	}}
	data := map[string]any{"country": ""}
	ApplyDefaults(form, data, "2026-10-17")
	if data["country"] != "Russia" || data["opened"] != "2026-10-17" || data["one"] != 1.0 {
		t.Errorf("defaults = %v", data)
	}
	data = map[string]any{"country": "Kazakhstan"}
	ApplyDefaults(form, data, "2026-10-17")
	if data["country"] != "Kazakhstan" {
		t.Errorf("a default replaced a given value: %v", data)
	}

	current := map[string]any{"code": "A-1", "country": "Russia", "one": 1.0}
	kept := KeepServerValues(form, current, map[string]any{"country": "Armenia", "code": "MINE"})
	if kept["code"] != "A-1" || kept["one"] != 1.0 || kept["country"] != "Armenia" {
		t.Errorf("a save not naming the hidden field: %v", kept)
	}
	cleared := KeepServerValues(form, current, map[string]any{"country": "Armenia", "one": nil})
	if v, ok := cleared["one"]; !ok || v != nil {
		t.Errorf("a save clearing the hidden field: %v", cleared)
	}
}

func TestEveryoneEditsUnlessReadOnly(t *testing.T) {
	other := RecordAccess{Reach: true}
	if p := other.Permissions(""); p.Edit || p.Delete {
		t.Errorf("a creator-only table let another person change a record: %+v", p)
	}
	team := RecordAccess{Reach: true, Everyone: true}
	if p := team.Permissions(""); !p.Edit || !p.Delete {
		t.Errorf("a table everyone edits refused another person: %+v", p)
	}
	ro := RecordAccess{Reach: true, Everyone: true, ReadOnly: true, Creator: true}
	if p := ro.Permissions(""); p.Edit || p.Delete {
		t.Errorf("a record naming a read-only member stayed changeable: %+v", p)
	}
	admin := RecordAccess{Reach: true, Admin: true, Everyone: true, ReadOnly: true}
	if p := admin.Permissions(""); !p.Edit || !p.Delete {
		t.Errorf("an administrator lost the right to change a record: %+v", p)
	}
	decided := RecordAccess{Reach: true, Everyone: true, Statuses: true}
	if p := decided.Permissions(StatusApproved); p.Edit {
		t.Errorf("everyone may change an approved record: %+v", p)
	}
}

func TestRelativeDateFilters(t *testing.T) {
	form := &FormDef{Label: "Tasks", Fields: []FormField{
		{Name: "title", Label: "Title", Type: "text"},
		{Name: "due", Label: "Due", Type: "date"},
	}}
	for _, f := range []Filter{
		{Field: "due", Op: "before_today"}, {Field: "due", Op: "on_or_before_today"}, {Field: "due", Op: "today"},
		{Field: "due", Op: "after_today"}, {Field: "due", Op: "next_days", Value: 7.0}, {Field: "updated_at", Op: "last_days", Value: 30.0},
	} {
		if err := CheckFilters(form, []Filter{f}); err != nil {
			t.Errorf("%s %s refused: %v", f.Field, f.Op, err)
		}
	}
	for _, f := range []Filter{
		{Field: "title", Op: "before_today"}, {Field: "due", Op: "next_days"}, {Field: "due", Op: "next_days", Value: -1.0},
		{Field: "due", Op: "last_days", Value: 2.5},
	} {
		if err := CheckFilters(form, []Filter{f}); err == nil || !IsQueryError(err) {
			t.Errorf("%s %s %v accepted (%v)", f.Field, f.Op, f.Value, err)
		}
	}
	b := &qb{form: form, today: "2026-10-17"}
	if err := b.filter(Filter{Field: "due", Op: "next_days", Value: 7.0}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.conds[0], "BETWEEN") || b.args[len(b.args)-2] != "2026-10-17" {
		t.Errorf("next_days condition %q args %v", b.conds[0], b.args)
	}
}
