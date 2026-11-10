package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// declarations lists a dimension's property names, sorted.
func (f *dimFormulaFixture) declarations() []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT name FROM model.dimension_property WHERE dimension_id=$1::uuid ORDER BY name`, f.region)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// memberProps is a region member's stored properties.
func (f *dimFormulaFixture) memberProps(code string) map[string]string {
	f.t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT COALESCE(properties,'{}'::jsonb) FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, f.region, code).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *dimFormulaFixture) importMembers(csv string) (int, map[string]any) {
	f.t.Helper()
	status, raw := f.req("POST", "/api/import/dimension-members", map[string]any{"dimension_id": f.region, "csv": csv})
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if status != http.StatusOK {
		out["raw"] = string(raw)
	}
	return status, out
}

// A property:<name> column of a member import fills the property declared
// under that name whatever its case — the importer lower-cases headers, and
// used to declare "segment" next to the developer's "Segment" and write the
// values where no formula looked. Any other header is declared as its
// identifier.
func TestMemberImportPropertyHeaderReusesTheDeclaration(t *testing.T) {
	f := setupDimFormulaFixture(t)
	props := "/api/developer/dimensions/" + f.region + "/properties"
	f.call("POST", props, map[string]any{"name": "Segment", "data_type": "text"})
	if _, err := f.pool.Exec(context.Background(), `UPDATE model.dimension_member SET properties='{"SEGMENT":"Old"}' WHERE dimension_id=$1::uuid AND code='EMEA'`, f.region); err != nil {
		t.Fatal(err)
	}

	if status, out := f.importMembers("code,label,property:SEGMENT\nEMEA,EMEA,Enterprise\n"); status != http.StatusOK {
		t.Fatalf("import property:SEGMENT: %d %v", status, out)
	}
	if got, want := f.declarations(), []string{"Segment"}; !reflect.DeepEqual(got, want) {
		t.Errorf("declarations after importing property:SEGMENT = %v, want %v", got, want)
	}
	if got, want := f.memberProps("EMEA"), map[string]string{"Segment": "Enterprise"}; !reflect.DeepEqual(got, want) {
		t.Errorf("EMEA holds %v, want %v", got, want)
	}

	if status, out := f.importMembers("code,label,property:Sales Segment\nUS,US,SMB\n"); status != http.StatusOK {
		t.Fatalf("import property:Sales Segment: %d %v", status, out)
	}
	if got, want := f.declarations(), []string{"Segment", "sales_segment"}; !reflect.DeepEqual(got, want) {
		t.Errorf("declarations after importing property:Sales Segment = %v, want %v", got, want)
	}
	if got, want := f.memberProps("US"), map[string]string{"sales_segment": "SMB"}; !reflect.DeepEqual(got, want) {
		t.Errorf("US holds %v, want %v", got, want)
	}

	if status, out := f.importMembers("code,property:%%\nUS,x\n"); status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out), metricformula.CodeInvalidPropertyName) {
		t.Errorf("import property:%%%%: %d %v, want 400 %s", status, out, metricformula.CodeInvalidPropertyName)
	}
}

// Deleting a property keeps its values (the owner's decision, 2026-11-09);
// declaring the name again says how many members hold values from before,
// and the developer can clear them from the property.
func TestPropertyRedeclaredOffersToClearItsValues(t *testing.T) {
	f := setupDimFormulaFixture(t)
	props := "/api/developer/dimensions/" + f.region + "/properties"
	factor := f.call("POST", props, map[string]any{"name": "factor", "data_type": "number"})
	f.setMember("EMEA", map[string]string{"factor": "2"})
	f.call("DELETE", props+"/"+factor, nil)
	if got, want := f.memberProps("EMEA"), map[string]string{"factor": "2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the delete EMEA holds %v, want %v (values are kept)", got, want)
	}

	status, raw := f.req("POST", props, map[string]any{"name": "FACTOR", "data_type": "number"})
	if status != http.StatusOK {
		t.Fatalf("declare FACTOR again: %d %s", status, raw)
	}
	var created struct {
		ID             string `json:"id"`
		ExistingValues int    `json:"existing_values"`
		Notice         string `json:"notice"`
	}
	_ = json.Unmarshal(raw, &created)
	if created.ExistingValues != 1 || !strings.Contains(created.Notice, `1 member(s) already hold values under "FACTOR" from before; formulas read them now`) {
		t.Errorf("declaring FACTOR again answered %s; want existing_values 1 and a notice saying so", raw)
	}
	if got, want := f.memberProps("EMEA"), map[string]string{"FACTOR": "2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after declaring FACTOR EMEA holds %v, want %v", got, want)
	}

	status, raw = f.req("PATCH", props+"/"+created.ID, map[string]any{"clear_values": true})
	if status != http.StatusOK || !strings.Contains(string(raw), `"values_cleared":1`) {
		t.Fatalf("clear FACTOR's values: %d %s; want 200 with values_cleared 1", status, raw)
	}
	if got := f.memberProps("EMEA"); len(got) != 0 {
		t.Errorf("after clearing EMEA holds %v, want nothing", got)
	}

	// An import declaring a name members hold values under says so too —
	// counting the members the file did not write.
	f.setMember("EMEA", map[string]string{"FACTOR": "7"})
	f.call("DELETE", props+"/"+created.ID, nil)
	status, out := f.importMembers("code,property:factor\nUS,5\n")
	if status != http.StatusOK || !strings.Contains(fmt.Sprint(out["notices"]), `1 member(s) already hold values under "factor" from before`) {
		t.Errorf("import declaring factor again: %d %v; want a notice about EMEA's value", status, out)
	}
}

// A member's value of a number or date property must read as one, on
// member writes, on a type change of the property and on an import.
func TestMemberPropertyValueMustMatchItsType(t *testing.T) {
	f := setupDimFormulaFixture(t)
	props := "/api/developer/dimensions/" + f.region + "/properties"
	f.call("POST", props, map[string]any{"name": "factor", "data_type": "number"})
	f.call("POST", props, map[string]any{"name": "launch", "data_type": "date"})
	segment := f.call("POST", props, map[string]any{"name": "segment", "data_type": "text"})
	member := "/api/developer/dimensions/" + f.region + "/members/"

	for _, tc := range []struct {
		props map[string]string
		code  string
	}{
		{map[string]string{"FACTOR": "abc"}, metricformula.CodeInvalidPropertyValue},
		{map[string]string{"launch": "2024-02-30"}, metricformula.CodeInvalidPropertyValue},
	} {
		f.refused(fmt.Sprintf("US %v", tc.props), "PATCH", member+f.members["US"], map[string]any{"code": "US", "label": "US", "properties": tc.props}, tc.code)
	}
	f.setMember("US", map[string]string{"factor": "", "launch": "2024-02-29", "segment": "SMB"})

	// A bad value kept from before does not block another edit.
	if _, err := f.pool.Exec(context.Background(), `UPDATE model.dimension_member SET properties='{"factor":"abc"}' WHERE dimension_id=$1::uuid AND code='EMEA'`, f.region); err != nil {
		t.Fatal(err)
	}
	f.call("PATCH", member+f.members["EMEA"], map[string]any{"code": "EMEA", "label": "Europe", "properties": map[string]string{"factor": "abc"}})

	f.refused("segment to number while US holds SMB", "PATCH", props+"/"+segment, map[string]any{"data_type": "number"}, metricformula.CodeInvalidPropertyValue)

	status, out := f.importMembers("code,property:factor\nUS,abc\n")
	if status != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out), metricformula.CodeInvalidPropertyValue) {
		t.Errorf("import factor=abc: %d %v, want the file refused (422 %s)", status, out, metricformula.CodeInvalidPropertyValue)
	}
	if got := f.memberProps("US")["factor"]; got != "" {
		t.Errorf("after the refused import US factor = %q, want it unchanged (empty)", got)
	}
}

// Two declarations or renames differing only in case, sent at once, leave
// one property: the database's unique index refuses the loser, answered
// 409 PROPERTY_NAME_TAKEN naming the winner — also when it lost before the
// write, to the pre-check (owner decision 2026-11-09: one status for both).
func TestPropertyNamesDifferingInCaseAreNotDeclaredAtOnce(t *testing.T) {
	f := setupDimFormulaFixture(t)
	props := "/api/developer/dimensions/" + f.region + "/properties"
	for i := 0; i < 15; i++ {
		pa := f.call("POST", props, map[string]any{"name": fmt.Sprintf("pa_%d", i), "data_type": "text"})
		pb := f.call("POST", props, map[string]any{"name": fmt.Sprintf("pb_%d", i), "data_type": "text"})
		type req struct {
			method, path, name string
		}
		reqs := []req{
			{"PATCH", props + "/" + pa, fmt.Sprintf("Seg_%d", i)},
			{"PATCH", props + "/" + pb, fmt.Sprintf("seg_%d", i)},
			{"POST", props, fmt.Sprintf("Tier_%d", i)},
			{"POST", props, fmt.Sprintf("tier_%d", i)},
		}
		statuses := make([]int, len(reqs))
		bodies := make([]string, len(reqs))
		var wg sync.WaitGroup
		for k, r := range reqs {
			wg.Add(1)
			go func(k int, r req) {
				defer wg.Done()
				status, raw := f.req(r.method, r.path, map[string]any{"name": r.name, "data_type": "text"})
				statuses[k], bodies[k] = status, string(raw)
			}(k, r)
		}
		wg.Wait()
		for _, lower := range []string{fmt.Sprintf("seg_%d", i), fmt.Sprintf("tier_%d", i)} {
			var n int
			if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM model.dimension_property WHERE dimension_id=$1::uuid AND lower(name)=$2`, f.region, lower).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("round %d: %d declarations named %s in some case, want 1", i, n, lower)
			}
		}
		for k := 0; k < len(reqs); k += 2 {
			ok := 0
			for _, j := range []int{k, k + 1} {
				switch {
				case statuses[j] == http.StatusOK:
					ok++
				case statuses[j] == http.StatusConflict && strings.Contains(bodies[j], metricformula.CodePropertyNameTaken):
				default:
					t.Errorf("round %d: %s %s answered %d %s", i, reqs[j].method, reqs[j].name, statuses[j], bodies[j])
				}
			}
			if ok != 1 {
				t.Errorf("round %d: %d of %s/%s succeeded, want 1", i, ok, reqs[k].name, reqs[k+1].name)
			}
		}
	}
}

// Two renames of one property sent at once: the second renames what the
// first left, so the members' values and the formulas end under the name
// the declaration has.
func TestConcurrentRenamesOfOnePropertyKeepItsValues(t *testing.T) {
	f := setupDimFormulaFixture(t)
	props := "/api/developer/dimensions/" + f.region + "/properties"
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("p_%d", i)
		id := f.call("POST", props, map[string]any{"name": name, "data_type": "number"})
		f.setMember("EMEA", map[string]string{name: "2"})
		var wg sync.WaitGroup
		for _, to := range []string{fmt.Sprintf("a_%d", i), fmt.Sprintf("b_%d", i)} {
			wg.Add(1)
			go func(to string) {
				defer wg.Done()
				if status, raw := f.req("PATCH", props+"/"+id, map[string]any{"name": to}); status != http.StatusOK {
					t.Errorf("rename %s to %s: %d %s", name, to, status, raw)
				}
			}(to)
		}
		wg.Wait()
		var final string
		if err := f.pool.QueryRow(context.Background(), `SELECT name FROM model.dimension_property WHERE id=$1::uuid`, id).Scan(&final); err != nil {
			t.Fatal(err)
		}
		got := f.memberProps("EMEA")
		if got[final] != "2" {
			t.Errorf("round %d: the declaration is %s but EMEA holds %v", i, final, got)
		}
		for _, other := range []string{name, fmt.Sprintf("a_%d", i), fmt.Sprintf("b_%d", i)} {
			if _, held := got[other]; held && other != final {
				t.Errorf("round %d: EMEA still holds a value under %s (the declaration is %s): %v", i, other, final, got)
			}
		}
	}
}
