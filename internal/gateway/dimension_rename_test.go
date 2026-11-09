package gateway

import (
	"context"
	"testing"
)

// renameFormulaFixture: region EMEA (factor 2) and US (factor 3), revenue
// EMEA=100 and US=50, and metrics reading the dimension and its members on
// the grid, every one computed.
func renameFormulaFixture(t *testing.T) *dimFormulaFixture {
	f := setupDimFormulaFixture(t)
	f.call("POST", "/api/developer/dimensions/"+f.region+"/properties", map[string]any{"name": "factor", "data_type": "number"})
	f.setMember("EMEA", map[string]string{"factor": "2"})
	f.setMember("US", map[string]string{"factor": "3"})
	for name, text := range map[string]string{
		"scaled":  `revenue * region.factor`,
		"lk":      `LOOKUP(revenue, region, "EMEA")`,
		"is_emea": `IF(region = "EMEA", 1, 0)`,
		"si_emea": `SUMIFS(revenue, region, "EMEA")`,
	} {
		f.metric[name] = f.call("POST", "/api/developer/metrics", f.metricBody(name, text))
		f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric[name], nil)
	}
	f.writeCell("revenue", "US", 50)
	f.writeCell("revenue", "EMEA", 100)
	f.await("scaled", "EMEA", 200)
	f.await("lk", "US", 100)
	f.await("is_emea", "EMEA", 1)
	f.await("si_emea", "US", 100)
	return f
}

func (f *dimFormulaFixture) formulaOf(name string) string {
	f.t.Helper()
	var text string
	if err := f.pool.QueryRow(context.Background(), `SELECT formula FROM model.metric_def WHERE id=$1::uuid`, f.metric[name]).Scan(&text); err != nil {
		f.t.Fatal(err)
	}
	return text
}

// TestMemberRenameKeepsFormulasReading: a member's new code reaches the
// formulas naming it, so they keep their values. They used to keep the old
// code and go silently wrong: is_emea 0, si_emea 0 and lk with no row.
func TestMemberRenameKeepsFormulasReading(t *testing.T) {
	f := renameFormulaFixture(t)
	f.call("PATCH", "/api/developer/dimensions/"+f.region+"/members/"+f.members["EMEA"], map[string]any{"code": "EUR", "label": "Europe"})
	for name, want := range map[string]string{
		"lk":      `LOOKUP(revenue, region, "EUR")`,
		"is_emea": `IF(region = "EUR", 1, 0)`,
		"si_emea": `SUMIFS(revenue, region, "EUR")`,
	} {
		if got := f.formulaOf(name); got != want {
			t.Errorf("%s reads %q, want %q", name, got, want)
		}
	}
	// A write recalculates them: what they read now comes from the
	// formulas, not from results re-keyed with the member.
	f.writeCell("revenue", "EUR", 120)
	f.await("si_emea", "US", 120)
	f.await("lk", "US", 120)
	f.await("is_emea", "EUR", 1)
	f.await("is_emea", "US", 0)
}

// TestDimensionRenameKeepsFormulasReading: a dimension's new name reaches
// the formulas naming it, so a later write computes them as before. They
// used to keep the old name: rows went missing or read 0.
func TestDimensionRenameKeepsFormulasReading(t *testing.T) {
	f := renameFormulaFixture(t)
	f.call("PATCH", "/api/developer/dimensions/"+f.region, map[string]any{"name": "territory"})
	for name, want := range map[string]string{
		"scaled":  `revenue * territory.factor`,
		"lk":      `LOOKUP(revenue, territory, "EMEA")`,
		"is_emea": `IF(territory = "EMEA", 1, 0)`,
		"si_emea": `SUMIFS(revenue, territory, "EMEA")`,
	} {
		if got := f.formulaOf(name); got != want {
			t.Errorf("%s reads %q, want %q", name, got, want)
		}
	}
	f.writeCell("revenue", "US", 60)
	f.await("scaled", "US", 180)
	f.await("scaled", "EMEA", 200)
	f.await("si_emea", "US", 100)
	f.await("lk", "US", 100)
	f.await("is_emea", "EMEA", 1)
}
