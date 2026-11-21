package crudapp

import (
	"slices"
	"strings"
	"testing"
)

// ticketTable is a help desk's tickets: a status with moves (Closed →
// Open only for managers), locked when Closed; a resolution required once
// Resolved; a resolved date the server stamps.
func ticketTable() *FormDef {
	return &FormDef{ID: "t", Fields: []FormField{
		{Name: "code", Type: FieldAutoCode, CodePrefix: "TKT-"},
		{Name: "title", Label: "Title", Type: "text", Required: true},
		{Name: "status", Label: "Status", Type: "select", Options: []string{"New", "Open", "Pending", "Resolved", "Closed"},
			Transitions: []Transition{
				{From: []string{"New"}, To: []string{"Open", "Resolved"}},
				{From: []string{"Open"}, To: []string{"Pending", "Resolved"}},
				{From: []string{"Pending"}, To: []string{"Open", "Resolved"}},
				{From: []string{"Resolved"}, To: []string{"Open", "Closed"}},
				{From: []string{"Closed"}, To: []string{"Open"}, Roles: []string{"Support managers"}},
			},
			LocksIn: []string{"Closed"}},
		{Name: "resolution", Label: "Resolution", Type: "longtext", RequiredWhen: &RowCondition{Field: "status", Is: []string{"Resolved", "Closed"}}},
		{Name: "resolved_on", Label: "Resolved on", Type: "date", StampWhen: &RowCondition{Field: "status", Is: []string{"Resolved"}}},
	}}
}

func TestFieldRulesValidate(t *testing.T) {
	if err := ValidateFields(ticketTable().Fields); err != nil {
		t.Fatalf("the ticket table's rules are valid: %v", err)
	}
	cases := map[string]FormField{
		"transitions are a select or dimension field's": {Name: "x", Type: "text", Transitions: []Transition{{From: []string{"a"}, To: []string{"b"}}}},
		"not one of its choices":                        {Name: "x", Type: "select", Options: []string{"a"}, Transitions: []Transition{{From: []string{"a"}, To: []string{"zz"}}}},
		"names no value":                                {Name: "x", Type: "select", Transitions: []Transition{{From: []string{"a"}}}},
		"stamp_when is a date field's":                  {Name: "x", Type: "text", StampWhen: &RowCondition{Field: "status", Is: []string{"Resolved"}}},
		"has no default":                                {Name: "x", Type: "date", Default: "today", StampWhen: &RowCondition{Field: "status", Is: []string{"Resolved"}}},
		"not a field of the table":                      {Name: "x", Type: "text", RequiredWhen: &RowCondition{Field: "nope", Is: []string{"a"}}},
		"active_flag is a boolean field's":              {Name: "x", Type: "text", ActiveFlag: true},
		"notify is a person field's":                    {Name: "x", Type: "text", Notify: true},
		"roles are a person field's":                    {Name: "x", Type: "text", Roles: []string{"Agents"}},
	}
	for want, f := range cases {
		fields := append(ticketTable().Fields, f)
		err := ValidateFields(fields)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	// A person field is a type of its own.
	if err := ValidateFields([]FormField{{Name: "owner", Type: FieldPerson, Roles: []string{"Agents"}, Notify: true}}); err != nil {
		t.Errorf("a person field: %v", err)
	}
}

func TestMovesAndRuleProblems(t *testing.T) {
	form := ticketTable()
	status := *form.Field("status")
	agent := func(roles []string) bool { return len(roles) == 0 }
	manager := func(roles []string) bool { return len(roles) == 0 || slices.Contains(roles, "Support managers") }

	if got := status.MovesFrom("New", agent); !slices.Equal(got, []string{"Open", "Resolved"}) {
		t.Errorf("moves from New: %v", got)
	}
	if got := status.MovesFrom("Closed", agent); len(got) != 0 {
		t.Errorf("an agent reopens no closed ticket: %v", got)
	}
	if got := status.MovesFrom("Closed", manager); !slices.Equal(got, []string{"Open"}) {
		t.Errorf("a manager reopens a closed ticket: %v", got)
	}

	cur := map[string]any{"title": "Printer", "status": "New"}
	if p := RuleProblems(form, cur, map[string]any{"title": "Printer", "status": "Closed"}, agent, nil); len(p) != 1 || !strings.Contains(p[0].Message, "cannot move from New to Closed") {
		t.Errorf("New → Closed is no move: %v", p)
	}
	if p := RuleProblems(form, cur, map[string]any{"title": "Printer", "status": "Open"}, agent, nil); len(p) != 0 {
		t.Errorf("New → Open is a move: %v", p)
	}
	// A new record starts anywhere; a record without a value takes any.
	if p := RuleProblems(form, nil, map[string]any{"status": "Closed"}, agent, nil); len(p) != 0 {
		t.Errorf("a new record: %v", p)
	}
	if p := RuleProblems(form, map[string]any{"title": "x"}, map[string]any{"title": "x", "status": "Closed"}, agent, nil); len(p) != 0 {
		t.Errorf("a record with no status: %v", p)
	}

	closed := map[string]any{"title": "Printer", "status": "Closed", "resolution": "Fixed"}
	if form.LockingField(closed) == nil {
		t.Fatal("a closed ticket is locked")
	}
	if p := RuleProblems(form, closed, map[string]any{"title": "Scanner", "status": "Closed", "resolution": "Fixed"}, manager, nil); len(p) != 1 || !strings.Contains(p[0].Message, "read-only while its Status is Closed") {
		t.Errorf("a closed ticket's title stays: %v", p)
	}
	if p := RuleProblems(form, closed, map[string]any{"title": "Printer", "status": "Open", "resolution": "Fixed"}, agent, nil); len(p) != 1 || !strings.Contains(p[0].Message, "only Support managers move Status from Closed to Open") {
		t.Errorf("an agent may not reopen: %v", p)
	}
	if p := RuleProblems(form, closed, map[string]any{"title": "Printer", "status": "Open", "resolution": "Fixed"}, manager, nil); len(p) != 0 {
		t.Errorf("a manager reopens: %v", p)
	}
	// A table without rules is never refused here.
	plain := &FormDef{Fields: []FormField{{Name: "status", Type: "select", Options: []string{"a", "b"}}}}
	if plain.HasFieldRules() || len(RuleProblems(plain, map[string]any{"status": "a"}, map[string]any{"status": "b"}, nil, nil)) != 0 {
		t.Error("a table without rules saves as before")
	}
}

func TestRequiredWhenAndStamps(t *testing.T) {
	form := ticketTable()
	if p := RequiredWhenMissing(form, map[string]any{"status": "Resolved"}, nil); len(p) != 1 || p[0].Field != "resolution" || !strings.Contains(p[0].Message, "required when Status is Resolved or Closed") {
		t.Errorf("a resolved ticket names its resolution: %v", p)
	}
	if p := RequiredWhenMissing(form, map[string]any{"status": "Open"}, nil); len(p) != 0 {
		t.Errorf("an open ticket needs none: %v", p)
	}

	today := "2026-11-20"
	data := map[string]any{"status": "Resolved"}
	ApplyStamps(form, map[string]any{"status": "Open"}, data, today)
	if data["resolved_on"] != today {
		t.Errorf("resolving stamps today: %v", data["resolved_on"])
	}
	// Staying resolved keeps the date; a typed one does not stick.
	data = map[string]any{"status": "Resolved", "resolved_on": "2020-01-01"}
	ApplyStamps(form, map[string]any{"status": "Resolved", "resolved_on": "2026-11-01"}, data, today)
	if data["resolved_on"] != "2026-11-01" {
		t.Errorf("the stamp stays: %v", data["resolved_on"])
	}
	// Reopened, it keeps the last resolved date until resolved again.
	data = map[string]any{"status": "Open"}
	ApplyStamps(form, map[string]any{"status": "Resolved", "resolved_on": "2026-11-01"}, data, today)
	if data["resolved_on"] != "2026-11-01" {
		t.Errorf("a reopened ticket keeps its date: %v", data["resolved_on"])
	}
	// A new record arriving resolved with a date (loaded from elsewhere) keeps it.
	data = map[string]any{"status": "Resolved", "resolved_on": "2026-03-04"}
	ApplyStamps(form, nil, data, today)
	if data["resolved_on"] != "2026-03-04" {
		t.Errorf("a loaded date stays: %v", data["resolved_on"])
	}
	// KeepServerValues keeps a stamped date a save sends.
	kept := KeepServerValues(form, map[string]any{"resolved_on": "2026-11-01"}, map[string]any{"resolved_on": "1999-01-01"})
	if kept["resolved_on"] != "2026-11-01" {
		t.Errorf("a save does not change a stamped date: %v", kept["resolved_on"])
	}
}

func TestLockAndDeleteNarrowPermissions(t *testing.T) {
	editor := RecordAccess{Reach: true, Creator: true}
	if !editor.Permissions("").Delete {
		t.Fatal("a creator deletes as before")
	}
	locked := editor
	locked.Locked = true
	if p := locked.Permissions(""); !p.Edit || p.Delete {
		t.Errorf("a locked record stays editable (its locking field moves) but is not deleted: %+v", p)
	}
	adminsOnly := editor
	adminsOnly.DeleteAdminsOnly = true
	if adminsOnly.Permissions("").Delete {
		t.Error("delete_access admins: an editor does not delete")
	}
	adminsOnly.DeleteRole = true
	if !adminsOnly.Permissions("").Delete {
		t.Error("an editor holding a delete role deletes")
	}
	admin := RecordAccess{Reach: true, Admin: true, Locked: true, DeleteAdminsOnly: true}
	if !admin.Permissions("").Delete {
		t.Error("an administrator deletes")
	}
	// Narrowing never widens: a reader holding a delete role deletes nothing.
	reader := RecordAccess{Reach: true, DeleteAdminsOnly: true, DeleteRole: true}
	if reader.Permissions("").Delete {
		t.Error("a delete role gives no one who may not change a record the right to delete it")
	}
}
