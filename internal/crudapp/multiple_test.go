package crudapp

import (
	"slices"
	"strings"
	"testing"
)

// A dimension field holding several members: the options it refuses, the
// values a record holds in it, and row conditions reading a list.
func TestMultipleField(t *testing.T) {
	dim := "8e0b7e7e-1111-4c8d-9a3e-0a0b0c0d0e0f"
	base := FormField{Name: "people", Label: "Contacts", Type: "dimension", DimensionID: dim, Multiple: true}
	if err := ValidateFields([]FormField{base}); err != nil {
		t.Fatalf("a plain field holding several: %v", err)
	}
	clear := base
	clear.OnDelete = OnDeleteClear
	if err := ValidateFields([]FormField{clear}); err != nil {
		t.Errorf("on_delete clear: %v", err)
	}
	for _, c := range []struct {
		edit func(*FormField)
		want string
	}{
		{func(f *FormField) { f.Type, f.DimensionID = "text", "" }, "dimension field's option"},
		{func(f *FormField) { f.OnDelete = OnDeleteCascade }, "takes a deleted member off its list"},
		{func(f *FormField) { f.Default = "A" }, "has no default"},
		{func(f *FormField) { f.LocksIn = []string{"A"} }, "moves and locks"},
		{func(f *FormField) { f.Notify = true }, "notify and roles"},
	} {
		f := base
		c.edit(&f)
		if err := ValidateFields([]FormField{f}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want an error naming %q", f, err, c.want)
		}
	}

	for _, c := range []struct {
		v    any
		want []string
	}{
		{nil, nil},
		{"A", []string{"A"}}, // a code saved before the field held several
		{[]any{"A", " B ", ""}, []string{"A", "B"}},
		{[]string{"C"}, []string{"C"}},
	} {
		if got := FieldValues(base, c.v); !slices.Equal(got, c.want) {
			t.Errorf("FieldValues(%v) = %v, want %v", c.v, got, c.want)
		}
	}

	data := map[string]any{"people": []any{"A", "B"}}
	for _, c := range []struct {
		cond RowCondition
		want bool
	}{
		{RowCondition{Field: "people", Is: []string{"b"}}, true},
		{RowCondition{Field: "people", Is: []string{"C"}}, false},
		{RowCondition{Field: "people", IsNot: []string{"A"}}, false},
		{RowCondition{Field: "people", IsNot: []string{"C"}}, true},
	} {
		if got := RowFilterMatches([]RowCondition{c.cond}, data); got != c.want {
			t.Errorf("%+v on [A B] = %v, want %v", c.cond, got, c.want)
		}
	}
	if !RowFilterMatches([]RowCondition{{Field: "people", Is: []string{""}}}, map[string]any{"people": []any{}}) {
		t.Errorf("an empty list meets \"is\" nothing")
	}
}
