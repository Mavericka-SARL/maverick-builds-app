package auditlog

import "testing"

func TestNullableArgs(t *testing.T) {
	cases := []struct {
		name                                      string
		fields                                    Fields
		wantActorNil, wantAppNil, wantRevisionNil bool
	}{
		{
			name:            "all empty (system event, e.g. scheduler fallback)",
			fields:          Fields{},
			wantActorNil:    true,
			wantAppNil:      true,
			wantRevisionNil: true,
		},
		{
			name:            "actor only (admin/user mgmt, no app or revision)",
			fields:          Fields{ActorUserID: "u1"},
			wantActorNil:    false,
			wantAppNil:      true,
			wantRevisionNil: true,
		},
		{
			name:            "app only (tenant/user mgmt call site)",
			fields:          Fields{ApplicationID: "a1"},
			wantActorNil:    true,
			wantAppNil:      false,
			wantRevisionNil: true,
		},
		{
			name:            "revision only (legacy pre-refactor call site shape)",
			fields:          Fields{RevisionID: "r1"},
			wantActorNil:    true,
			wantAppNil:      true,
			wantRevisionNil: false,
		},
		{
			name:            "fully populated (developer console mutation)",
			fields:          Fields{ActorUserID: "u1", ApplicationID: "a1", RevisionID: "r1"},
			wantActorNil:    false,
			wantAppNil:      false,
			wantRevisionNil: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actor, app, revision := nullableArgs(tc.fields)
			if (actor == nil) != tc.wantActorNil {
				t.Errorf("actor nil = %v, want %v", actor == nil, tc.wantActorNil)
			}
			if (app == nil) != tc.wantAppNil {
				t.Errorf("app nil = %v, want %v", app == nil, tc.wantAppNil)
			}
			if (revision == nil) != tc.wantRevisionNil {
				t.Errorf("revision nil = %v, want %v", revision == nil, tc.wantRevisionNil)
			}
			if actor != nil && *actor != tc.fields.ActorUserID {
				t.Errorf("actor = %q, want %q", *actor, tc.fields.ActorUserID)
			}
			if app != nil && *app != tc.fields.ApplicationID {
				t.Errorf("app = %q, want %q", *app, tc.fields.ApplicationID)
			}
			if revision != nil && *revision != tc.fields.RevisionID {
				t.Errorf("revision = %q, want %q", *revision, tc.fields.RevisionID)
			}
		})
	}
}
