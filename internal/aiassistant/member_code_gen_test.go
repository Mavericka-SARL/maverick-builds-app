package aiassistant_test

// A dimension built from a file with no codes: the AI Developer leaves
// "code" out and the engine makes one from the label, at most 10 characters
// (modeledit.MemberCode), as a file import does. Two members listed without
// a code used to share the empty code, and the second vanished silently.

import "testing"

func TestMembersWithoutCodesGetGeneratedCodes(t *testing.T) {
	h := newEditHarness(t)
	_, product := h.must("create_dimension", map[string]any{"name": "product", "members": []map[string]any{
		{"label": "Lipitor"},
		{"label": "Lipitor 10 mg tablets", "parent_code": "Lipitor"},
		{"label": "Lipitor 20 mg tablets", "parent_code": "Lipitor"},
		{"code": "CRESTOR", "label": "Crestor"},
	}})
	codes := func() string {
		t.Helper()
		return h.scalar(`SELECT string_agg(m.code || '<' || COALESCE(p.code, ''), ',' ORDER BY m.code)
			FROM model.dimension_member m LEFT JOIN model.dimension_member p ON p.id = m.parent_member_id
			WHERE m.dimension_id=$1::uuid`, product)
	}
	if got, want := codes(), "CRESTOR<,LIPITOR<,LIPITOR_10<LIPITOR,LIPITOR_20<LIPITOR"; got != want {
		t.Errorf("codes after create_dimension: %s, want %s", got, want)
	}

	// Added one by one: a free code, the parent named by its label.
	h.must("add_dimension_member", map[string]any{"dimension_id": "product", "label": "Lipitor 10 mg tablets (new pack)", "parent_code": "Lipitor"})
	if got, want := codes(), "CRESTOR<,LIPITOR<,LIPITOR_10<LIPITOR,LIPITOR_2<LIPITOR,LIPITOR_20<LIPITOR"; got != want {
		t.Errorf("codes after add_dimension_member: %s, want %s", got, want)
	}

	_, _, err := h.run("add_dimension_member", map[string]any{"dimension_id": "product"})
	h.refused("neither a code nor a label", err, "a code or label")
}
