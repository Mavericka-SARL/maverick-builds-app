package aiassistant_test

// reorder_dimension_members is the AI Developer's twin of
// PUT /api/developer/dimensions/{id}/members/order: both run
// modeledit.ReorderMembers, so they accept and refuse the same requests and
// leave the same order behind.

import (
	"testing"

	"github.com/mavericks-engine/mavericks/internal/modeledit"
)

func TestReorderDimensionMembers(t *testing.T) {
	h := newEditHarness(t)
	_, region := h.must("create_dimension", map[string]any{"name": "region", "members": []map[string]any{
		{"code": "EMEA", "label": "EMEA"}, {"code": "APAC", "label": "APAC"}, {"code": "AMER", "label": "AMER"},
		{"code": "UK", "label": "UK", "parent_code": "EMEA"}, {"code": "DE", "label": "DE", "parent_code": "EMEA"},
		{"code": "JP", "label": "JP", "parent_code": "APAC"}}})
	order := func() string {
		t.Helper()
		return h.scalar(`SELECT string_agg(code, ',' ORDER BY sort_order, code) FROM model.dimension_member WHERE dimension_id=$1::uuid`, region)
	}

	// The top level: each member's children follow it.
	res, _ := h.must("reorder_dimension_members", map[string]any{"dimension_id": "region", "codes": []string{"AMER", "EMEA", "APAC"}})
	if res != "Top-level members reordered: AMER, EMEA, APAC" {
		t.Errorf("result %q", res)
	}
	if got, want := order(), "AMER,EMEA,UK,DE,APAC,JP"; got != want {
		t.Errorf("after reordering the top level: %s, want %s", got, want)
	}
	// A nested level.
	h.must("reorder_dimension_members", map[string]any{"dimension_id": region, "parent_code": "EMEA", "codes": []string{"DE", "UK"}})
	if got, want := order(), "AMER,EMEA,DE,UK,APAC,JP"; got != want {
		t.Errorf("after reordering EMEA's children: %s, want %s", got, want)
	}

	// Refused, leaving the order as it was.
	for _, tc := range []struct {
		what   string
		params map[string]any
		want   string
	}{
		{"a member left out", map[string]any{"dimension_id": region, "codes": []string{"AMER", "EMEA"}}, "missing: APAC"},
		{"a member twice", map[string]any{"dimension_id": region, "codes": []string{"AMER", "EMEA", "EMEA", "APAC"}}, "listed more than once"},
		{"a child among the top level", map[string]any{"dimension_id": region, "codes": []string{"AMER", "EMEA", "APAC", "UK"}}, "is not one of the top-level members"},
		{"an unknown code", map[string]any{"dimension_id": region, "codes": []string{"AMER", "EMEA", "LATAM"}}, `member "LATAM" not found`},
		{"an unknown parent", map[string]any{"dimension_id": region, "parent_code": "NOPE", "codes": []string{}}, `parent member "NOPE" not found`},
		{"no codes", map[string]any{"dimension_id": region}, "codes"},
	} {
		_, _, err := h.run("reorder_dimension_members", tc.params)
		h.refused(tc.what, err, tc.want)
	}
	if got, want := order(), "AMER,EMEA,DE,UK,APAC,JP"; got != want {
		t.Errorf("a refused reorder changed the order: %s, want %s", got, want)
	}

	// A dimension whose members' parents sit in its parent dimension: the
	// reordered group takes the positions it held, the others stay put.
	city := h.scalar(`INSERT INTO model.dimension_def (model_id, revision_id, name, parent_dimension_id)
		VALUES ($1::uuid, $2::uuid, 'city', $3::uuid) RETURNING id::text`, h.modelID, h.revID, region)
	for i, c := range []struct{ code, parent string }{{"LON", "EMEA"}, {"TOK", "APAC"}, {"PAR", "EMEA"}, {"BER", "EMEA"}} {
		h.scalar(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order)
			SELECT $1::uuid, $2, $2, id, $4 FROM model.dimension_member WHERE dimension_id=$3::uuid AND code=$5 RETURNING id::text`,
			city, c.code, region, i+1, c.parent)
	}
	h.must("reorder_dimension_members", map[string]any{"dimension_id": "city", "parent_code": "EMEA", "codes": []string{"BER", "PAR", "LON"}})
	if got := h.scalar(`SELECT string_agg(code, ',' ORDER BY sort_order, code) FROM model.dimension_member WHERE dimension_id=$1::uuid`, city); got != "BER,TOK,PAR,LON" {
		t.Errorf("cities after reordering EMEA's: %s, want BER,TOK,PAR,LON", got)
	}

	// Which side a level's parent sits on follows the members, not the
	// declared parent dimension, which a developer can clear or set after
	// the members exist. city's parent dimension cleared: EMEA still parents
	// LON, PAR and BER.
	h.scalar(`UPDATE model.dimension_def SET parent_dimension_id=NULL WHERE id=$1::uuid RETURNING id::text`, city)
	h.must("reorder_dimension_members", map[string]any{"dimension_id": city, "parent_code": "EMEA", "codes": []string{"LON", "PAR", "BER"}})
	if got := h.scalar(`SELECT string_agg(code, ',' ORDER BY sort_order, code) FROM model.dimension_member WHERE dimension_id=$1::uuid`, city); got != "LON,TOK,PAR,BER" {
		t.Errorf("cities after clearing the parent dimension and reordering EMEA's: %s, want LON,TOK,PAR,BER", got)
	}
	// team's own hierarchy, then region declared its parent dimension: LEAD
	// still parents A and B.
	_, team := h.must("create_dimension", map[string]any{"name": "team", "members": []map[string]any{
		{"code": "LEAD", "label": "Lead"}, {"code": "A", "label": "A", "parent_code": "LEAD"}, {"code": "B", "label": "B", "parent_code": "LEAD"}}})
	h.scalar(`UPDATE model.dimension_def SET parent_dimension_id=$2::uuid WHERE id=$1::uuid RETURNING id::text`, team, region)
	h.must("reorder_dimension_members", map[string]any{"dimension_id": team, "parent_code": "LEAD", "codes": []string{"B", "A"}})
	if got := h.scalar(`SELECT string_agg(code, ',' ORDER BY sort_order, code) FROM model.dimension_member WHERE dimension_id=$1::uuid`, team); got != "LEAD,B,A" {
		t.Errorf("team after declaring a parent dimension and reordering LEAD's: %s, want LEAD,B,A", got)
	}

	// A time dimension keeps calendar order.
	months := h.scalar(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, 'months', 'time', 'month', 1) RETURNING id::text`, h.modelID, h.revID)
	h.scalar(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index)
		VALUES ($1::uuid, 'JAN', 'Jan', '2026-01-01', '2026-01-31', 0) RETURNING id::text`, months)
	_, _, err := h.run("reorder_dimension_members", map[string]any{"dimension_id": months, "codes": []string{"JAN"}})
	h.refused("a time dimension", err, modeledit.ErrTimeOrder)

	// Another model's dimension is refused.
	other := seedModel(t, h.pool)
	theirs := h.scalar(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'x') RETURNING id::text`, other, seedRevision(t, h.pool, other, "O"))
	_, _, err = h.run("reorder_dimension_members", map[string]any{"dimension_id": theirs, "codes": []string{}})
	h.refused("another model's dimension", err, "different model")
}
