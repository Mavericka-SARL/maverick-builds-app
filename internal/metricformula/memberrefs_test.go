package metricformula

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestFormulaNamesMember(t *testing.T) {
	for _, tc := range []struct {
		text      string
		timeCodes bool
		want      bool
	}{
		// LOOKUP: a literal member of this dimension, any level, exactly.
		{`LOOKUP(revenue, region, "EMEA")`, false, true},
		{`LOOKUP(revenue, REGION, "EMEA")`, false, true},
		{`LOOKUP(revenue, region, "World")`, false, false},
		{`LOOKUP(revenue, region, "emea")`, false, false}, // LOOKUP resolves codes exactly
		{`LOOKUP(revenue, product, "EMEA")`, false, false},
		{`LOOKUP(revenue, product, "P1", region, "EMEA")`, false, true},
		{`LOOKUP(revenue, region, PARENT(region))`, false, false},
		// *IFS / *IF: an equality criterion on the code range.
		{`SUMIFS(revenue, region, "EMEA")`, false, true},
		{`SUMIFS(revenue, region, "emea")`, false, true}, // criteria match case-insensitively
		{`SUMIFS(revenue, region, "=EMEA")`, false, true},
		{`SUMIF(region, "EMEA", revenue)`, false, true},
		{`COUNTIFS(region, "EMEA", product, "P1")`, false, true},
		{`SUMIFS(revenue, region, "<>EMEA")`, false, false},
		{`SUMIFS(revenue, region, "EM*")`, false, false},
		{`SUMIFS(revenue, region, ">EMEA")`, false, false},
		{`SUMIFS(revenue, region.segment, "EMEA")`, false, false}, // a property range
		{`SUMIFS(revenue, product, "EMEA")`, false, false},
		// TIMESUM start / end codes, only on the time dimension.
		{`TIMESUM(revenue, "EMEA", "2026-03")`, true, true},
		{`TIMESUM(revenue, "2026-01", "EMEA")`, true, true},
		{`TIMESUM(revenue, "EMEA", "2026-03")`, false, false},
		{`TIMESUM(revenue)`, true, false},
		// Comparisons with the bare dimension or PARENT(dim).
		{`IF(region = "EMEA", revenue, 0)`, false, true},
		{`IF("emea" = region, revenue, 0)`, false, true},
		{`IF(region <> "EMEA", revenue, 0)`, false, true},
		{`IF(PARENT(region) = "EMEA", revenue, 0)`, false, true},
		{`IF(region > "EMEA", revenue, 0)`, false, false},
		{`IF(region.segment = "EMEA", revenue, 0)`, false, false},
		{`IF(product = "EMEA", revenue, 0)`, false, false},
		// SWITCH on the dimension compares each match value as "=" does.
		{`SWITCH(region, "EMEA", 1, 0) * revenue`, false, true},
		{`SWITCH(region, "APAC", 1, "emea", 2, 0)`, false, true},
		{`SWITCH(PARENT(region), "EMEA", 1, 0)`, false, true},
		{`SWITCH(region, "APAC", 1, "EMEA")`, false, false}, // "EMEA" is the default result
		{`SWITCH(region, "APAC", "EMEA", 0)`, false, false}, // a result, not a match value
		{`SWITCH(product, "EMEA", 1, 0)`, false, false},
		{`SWITCH(region.segment, "EMEA", 1, 0)`, false, false},
		{`IF(revenue > 0, SWITCH(region, "EMEA", 1, 0), 0)`, false, true},
		// A string that only mentions the code.
		{`IF(region = "EMEA2", revenue, 0)`, false, false},
		{`revenue * 2`, false, false},
		{`LOOKUP(`, false, false}, // unparsable
	} {
		if got := FormulaNamesMember(tc.text, "region", "EMEA", tc.timeCodes); got != tc.want {
			t.Errorf("FormulaNamesMember(%q, region, EMEA, time=%v) = %v, want %v", tc.text, tc.timeCodes, got, tc.want)
		}
	}
	// A numeric code is named by a number literal too.
	if !FormulaNamesMember(`IF(region = 100, 1, 0)`, "region", "100", false) {
		t.Error(`region = 100 should name member 100`)
	}
	if !FormulaNamesMember(`SUMIFS(revenue, region, 100)`, "region", "100", false) {
		t.Error(`SUMIFS criterion 100 should name member 100`)
	}
}

func TestCheckMemberNotInUse(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	memberID := func(dimID, code string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dimID, code).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	addMetric := func(name, text string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula)
			VALUES ($1::uuid, $2::uuid, $3, false, $4) RETURNING id::text`, f.modelID, f.revID, name, text).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	emea, us := memberID(f.region, "EMEA"), memberID(f.region, "US")
	if err := CheckMemberNotInUse(ctx, f.pool, f.region, emea); err != nil {
		t.Fatalf("no formula names EMEA yet: %v", err)
	}
	addMetric("lk_emea", `LOOKUP(revenue, region, "EMEA")`)
	addMetric("emea_flag", `IF(region = "emea", 1, 0)`)
	err := CheckMemberNotInUse(ctx, f.pool, f.region, emea)
	if codeOf(err) != CodeMemberInUse || !strings.Contains(err.Error(), "lk_emea") || !strings.Contains(err.Error(), "emea_flag") {
		t.Errorf("delete EMEA: got %v, want %s naming lk_emea and emea_flag", err, CodeMemberInUse)
	}
	if err := CheckMemberNotInUse(ctx, f.pool, f.region, us); err != nil {
		t.Errorf("US is named by no formula: %v", err)
	}
	// A member of another dimension, or of none, is not found.
	if err := CheckMemberNotInUse(ctx, f.pool, f.product, emea); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("EMEA under product: got %v, want ErrNoRows", err)
	}

	// TIMESUM codes count for a metric on a grid carrying the time dimension.
	jan := memberID(f.month, "2026-01")
	ts := addMetric("ytd", `TIMESUM(revenue, "2026-01", "2026-03")`)
	if err := CheckMemberNotInUse(ctx, f.pool, f.month, jan); err != nil {
		t.Errorf("ytd is on no grid with month: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, f.g1, ts); err != nil {
		t.Fatal(err)
	}
	if err := CheckMemberNotInUse(ctx, f.pool, f.month, jan); codeOf(err) != CodeMemberInUse || !strings.Contains(err.Error(), "ytd") {
		t.Errorf("delete 2026-01 with ytd on G1: got %v, want %s naming ytd", err, CodeMemberInUse)
	}
	if err := CheckMemberNotInUse(ctx, f.pool, f.month, memberID(f.month, "2026-02")); err != nil {
		t.Errorf("2026-02 is named by no formula: %v", err)
	}
}
