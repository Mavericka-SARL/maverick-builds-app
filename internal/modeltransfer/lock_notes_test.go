package modeltransfer

import (
	"strings"
	"testing"
)

// TestLockNotesNameTheReopenRoles: the import note about a carried approval
// says who can reopen it. When the workflow names reopen roles only they can
// (an administrator's Reopen is refused), so the note names them; otherwise
// a business admin can (2026-11-19: the note sent everyone to the admins).
func TestLockNotesNameTheReopenRoles(t *testing.T) {
	p := Package{
		Workflows: []Workflow{
			{Name: "Annual budget approval", ReopenRoles: []string{"CEO"}},
			{Name: "Month-end close", ReopenRoles: []string{"CFO", "Controller"}},
			{Name: "Quota book"},
		},
		ApprovedLocks: []ApprovedLock{
			{Workflow: "Annual budget approval", Scope: []string{"Budget case = BUDGET"}},
			{Workflow: "Month-end close", Scope: []string{"Month to close = 2026-10"}},
			{Workflow: "Quota book", Scope: []string{"Plan year = FY2027"}},
			{Workflow: "Quota book", Scope: []string{"Plan year = FY2028"}},
		},
	}
	notes := p.LockNotes([]bool{true, true, true, false})
	if len(notes) != 4 {
		t.Fatalf("notes = %q, want one per approval", notes)
	}
	for i, want := range []string{
		"Only CEO can reopen it: on their Workflow page, with a reason.",
		"Only CFO or Controller can reopen it: on their Workflow page, with a reason.",
		"A business admin can reopen it in Business Admin › Workflow, with a reason.",
		"open here until \"Quota book\" is run for them again",
	} {
		if !strings.Contains(notes[i], want) {
			t.Errorf("note %d = %q, want it to say %q", i, notes[i], want)
		}
	}
	if strings.Contains(notes[0], "business admin") {
		t.Errorf("note %q sends the importer to a business admin, who may not reopen it", notes[0])
	}
}
