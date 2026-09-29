package crudapp_test

import (
	"reflect"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// TestRecordPermissionsRule pins the "submitter + admins" rule for every
// kind of caller and every status.
func TestRecordPermissionsRule(t *testing.T) {
	none := crudapp.RecordPermissions{SetStatus: []string{}}
	cases := []struct {
		name   string
		access crudapp.RecordAccess
		status string
		want   crudapp.RecordPermissions
	}{
		{"unreached admin", crudapp.RecordAccess{Admin: true, Creator: true}, "draft", none},
		{"reader of a draft", crudapp.RecordAccess{Reach: true}, "draft", none},
		{"reader of a submitted record", crudapp.RecordAccess{Reach: true}, "submitted", none},
		{"creator of a draft", crudapp.RecordAccess{Reach: true, Creator: true}, "draft",
			crudapp.RecordPermissions{Edit: true, Delete: true, SetStatus: []string{"submitted"}}},
		{"creator of a submitted record", crudapp.RecordAccess{Reach: true, Creator: true}, "submitted",
			crudapp.RecordPermissions{Edit: true, Delete: true, SetStatus: []string{"draft"}}},
		{"creator of an approved record", crudapp.RecordAccess{Reach: true, Creator: true}, "approved", none},
		{"creator of a rejected record", crudapp.RecordAccess{Reach: true, Creator: true}, "rejected", none},
		{"admin of a submitted record", crudapp.RecordAccess{Reach: true, Admin: true}, "submitted",
			crudapp.RecordPermissions{Edit: true, Delete: true, SetStatus: []string{"draft", "approved", "rejected"}}},
		{"admin of an approved record", crudapp.RecordAccess{Reach: true, Admin: true}, "approved",
			crudapp.RecordPermissions{Edit: true, Delete: true, SetStatus: []string{"draft", "submitted", "rejected"}}},
		{"admin who created an approved record", crudapp.RecordAccess{Reach: true, Admin: true, Creator: true}, "approved",
			crudapp.RecordPermissions{Edit: true, Delete: true, SetStatus: []string{"draft", "submitted", "rejected"}}},
	}
	for _, c := range cases {
		if got := c.access.Permissions(c.status); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Permissions(%q) = %+v, want %+v", c.name, c.status, got, c.want)
		}
	}
}

func TestRecordAccessChecks(t *testing.T) {
	creator := crudapp.RecordAccess{Reach: true, Creator: true}
	reader := crudapp.RecordAccess{Reach: true}
	admin := crudapp.RecordAccess{Reach: true, Admin: true}

	checks := []struct {
		name string
		got  bool
		want bool
	}{
		{"creator saves own submitted record", creator.CanUpdate("submitted", "submitted"), true},
		{"creator withdraws to draft", creator.CanUpdate("submitted", "draft"), true},
		{"creator submits draft", creator.CanUpdate("draft", "submitted"), true},
		{"creator approves own record", creator.CanUpdate("submitted", "approved"), false},
		{"creator rejects own record", creator.CanUpdate("draft", "rejected"), false},
		{"creator edits own approved record", creator.CanUpdate("approved", "approved"), false},
		{"creator reopens own approved record", creator.CanUpdate("approved", "draft"), false},
		{"creator deletes own submitted record", creator.CanDelete("submitted"), true},
		{"creator deletes own approved record", creator.CanDelete("approved"), false},
		{"reader edits", reader.CanUpdate("submitted", "submitted"), false},
		{"reader deletes", reader.CanDelete("draft"), false},
		{"admin approves", admin.CanUpdate("submitted", "approved"), true},
		{"admin reopens approved", admin.CanUpdate("approved", "draft"), true},
		{"admin deletes approved", admin.CanDelete("approved"), true},
		{"admin sets unknown status", admin.CanUpdate("submitted", "closed"), false},
		{"reader creates draft", reader.CanCreate("draft"), true},
		{"reader creates submitted", reader.CanCreate("submitted"), true},
		{"reader creates approved", reader.CanCreate("approved"), false},
		{"admin creates approved", admin.CanCreate("approved"), true},
		{"admin creates unknown status", admin.CanCreate("closed"), false},
		{"unreached creates draft", crudapp.RecordAccess{}.CanCreate("draft"), false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}
