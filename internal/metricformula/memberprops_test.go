package metricformula

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPropertyKey(t *testing.T) {
	for in, want := range map[string]string{
		"Sales Segment": "sales_segment",
		"sales_segment": "sales_segment",
		"SALES-SEGMENT": "sales_segment",
		" P&L  Line ":   "p_l_line",
		"_tier_":        "tier",
		"Größe":         "gr_e",
		"%%":            "",
	} {
		if got := PropertyKey(in); got != want {
			t.Errorf("PropertyKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// An import's property:<name> header lands on the declaration of that name
// whatever its case, or of its identifier; any other header is declared as
// its identifier. A header with no letter or digit, or two headers filling
// one property, refuse the file.
func TestImportedPropertyNames(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.dimension_property (dimension_id, name) VALUES ($1::uuid, 'Sales_Region')`, f.region); err != nil {
		t.Fatal(err)
	}
	names, toDeclare, err := ImportedPropertyNames(ctx, f.pool, f.region, []string{"SEGMENT", "Factor", "sales region", "Sales Segment", "2nd tier"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SEGMENT": "segment", "Factor": "factor", "sales region": "Sales_Region", "Sales Segment": "sales_segment", "2nd tier": "p_2nd_tier"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if want := []string{"sales_segment", "p_2nd_tier"}; !reflect.DeepEqual(toDeclare, want) {
		t.Errorf("toDeclare = %v, want %v", toDeclare, want)
	}
	for _, tc := range []struct {
		headers []string
		says    string
	}{
		{[]string{"%%"}, "no letter or digit"},
		{[]string{"Sales Segment", "sales-segment"}, "both fill"},
		{[]string{"segment", "Segment"}, "both fill"},
	} {
		if _, _, err := ImportedPropertyNames(ctx, f.pool, f.region, tc.headers); err == nil || !IsValidationError(err) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("headers %q: err %v, want a validation error saying %q", tc.headers, err, tc.says)
		}
	}
}

// Declaring an import's properties declares the new ones as text, moves
// values kept under another case under them, and reports how many members
// held a value under each new name from before.
func TestDeclareImportedProperties(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE model.dimension_member SET properties = properties || '{"Tier":"Gold"}' WHERE dimension_id=$1::uuid AND code='EMEA'`, f.region); err != nil {
		t.Fatal(err)
	}
	names, declared, err := DeclareImportedProperties(ctx, f.pool, f.region, []string{"tier", "segment", "Sales Segment"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"tier": "tier", "segment": "segment", "Sales Segment": "sales_segment"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if want := []string{"tier", "sales_segment"}; !reflect.DeepEqual(declared, want) {
		t.Errorf("declared = %v, want %v", declared, want)
	}
	var props []byte
	if err := f.pool.QueryRow(ctx, `SELECT properties FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='EMEA'`, f.region).Scan(&props); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(props), `"tier": "Gold"`) || strings.Contains(string(props), `"Tier"`) {
		t.Errorf("EMEA holds %s, want Tier moved under tier", props)
	}
	n, err := ValuesFromBefore(ctx, f.pool, f.region, "tier", nil)
	if err != nil || n != 1 {
		t.Errorf("ValuesFromBefore(tier) = %d, %v; want 1", n, err)
	}
	if n, err := ValuesFromBefore(ctx, f.pool, f.region, "tier", []string{"EMEA"}); err != nil || n != 0 {
		t.Errorf("ValuesFromBefore(tier) without EMEA, which the import rewrote = %d, %v; want 0", n, err)
	}
}

// A member's value of a number or date property must read as one; a blank
// is allowed, and a value the member already holds passes unchanged.
func TestCheckMemberPropertiesChecksTypes(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid, 'launch', 'date')`, f.region); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		props, stored map[string]string
		code          string
	}{
		{map[string]string{"factor": "abc"}, nil, CodeInvalidPropertyValue},
		{map[string]string{"FACTOR": "abc"}, nil, CodeInvalidPropertyValue},
		{map[string]string{"launch": "2024-02-30"}, nil, CodeInvalidPropertyValue},
		{map[string]string{"factor": "2.5", "launch": "2024-02-29"}, nil, ""},
		{map[string]string{"factor": ""}, nil, ""},
		{map[string]string{"factor": "abc", "segment": "SMB"}, map[string]string{"factor": "abc"}, ""},
		{map[string]string{"segment": "anything"}, nil, ""},
	} {
		_, err := CheckMemberProperties(ctx, f.pool, f.region, tc.props, tc.stored)
		if got := codeOf(err); got != tc.code || (tc.code == "" && err != nil) {
			t.Errorf("CheckMemberProperties(%v, stored %v): %v, want code %q", tc.props, tc.stored, err, tc.code)
		}
	}
	// Retyping a property is refused while a stored value would not read.
	if err := CheckPropertyValues(ctx, f.pool, f.region, "segment", "number"); codeOf(err) != CodeInvalidPropertyValue || !strings.Contains(err.Error(), "EMEA") {
		t.Errorf("retype segment to number: %v, want %s naming EMEA", err, CodeInvalidPropertyValue)
	}
	if err := CheckPropertyValues(ctx, f.pool, f.region, "factor", "text"); err != nil {
		t.Errorf("retype factor to text: %v", err)
	}
}

// Clearing a property's values removes the key in any case from every
// member of the dimension, and nothing else.
func TestClearPropertyValues(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE model.dimension_member SET properties = properties || '{"FACTOR":"9"}' WHERE dimension_id=$1::uuid AND code='World'`, f.region); err != nil {
		t.Fatal(err)
	}
	n, err := ClearPropertyValues(ctx, f.pool, f.region, "Factor")
	if err != nil || n != 3 {
		t.Fatalf("ClearPropertyValues = %d, %v; want 3 members", n, err)
	}
	rows, err := f.pool.Query(ctx, `SELECT properties FROM model.dimension_member WHERE dimension_id=$1::uuid ORDER BY code`, f.region)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []map[string]string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		_ = json.Unmarshal(raw, &m)
		got = append(got, m)
	}
	want := []map[string]string{{"segment": "SMB"}, {"segment": "ENT"}, {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("members hold %v, want %v", got, want)
	}
}

// A declaration that loses a race to one differing only in case is refused
// by the database; the error names the property that won.
func TestPropertyNameConflict(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `INSERT INTO model.dimension_property (dimension_id, name) VALUES ($1::uuid, 'FACTOR')`, f.region)
	if err == nil {
		t.Fatal("FACTOR was declared next to factor")
	}
	got := PropertyNameConflict(ctx, f.pool, f.region, "FACTOR", err)
	if codeOf(got) != CodePropertyNameTaken || !strings.Contains(got.Error(), `"factor"`) || !IsPropertyNameConflict(got) {
		t.Errorf("PropertyNameConflict = %v, want %s naming factor", got, CodePropertyNameTaken)
	}
	if other := PropertyNameConflict(ctx, f.pool, f.region, "FACTOR", context.Canceled); other != context.Canceled || IsPropertyNameConflict(other) {
		t.Errorf("another error came back as %v", other)
	}
}
