package scim

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTokenRoundTrip(t *testing.T) {
	const cust = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	plain, hash, err := NewToken(cust)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "mvx_scim_0b1c2d3e4f5061728394a5b6c7d8e9f0_") {
		t.Fatalf("token shape: %s", plain)
	}
	if got, ok := ParseToken(plain); !ok || got != cust {
		t.Fatalf("parse: %q %v", got, ok)
	}
	if HashToken(plain) != hash {
		t.Fatal("hash mismatch")
	}
	// The tenant prefix is part of what is hashed: swapping it in cannot
	// reuse another tenant's secret.
	swapped := "mvx_scim_ffffffffffffffffffffffffffffffff_" + strings.SplitN(plain, "_", 4)[3]
	if HashToken(swapped) == hash {
		t.Fatal("a re-prefixed token hashed the same")
	}
	for _, bad := range []string{"", "Bearer x", "mvx_scim_short_x", "mvx_scim_0b1c2d3e4f5061728394a5b6c7d8e9f0_tiny"} {
		if _, ok := ParseToken(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestFilterParsing(t *testing.T) {
	cases := map[string][]clause{
		``:                                       nil,
		`userName eq "a@b.c"`:                    {{"username", "a@b.c"}},
		`externalId eq "x-1" and active eq true`: {{"externalid", "x-1"}, {"active", "true"}},
		`emails[type eq "work"].value eq "a@b.c"`:                        {{"emails.value", "a@b.c"}},
		`urn:ietf:params:scim:schemas:core:2.0:User:userName eq "a@b.c"`: {{"username", "a@b.c"}},
		`displayName eq "Finance Review"`:                                {{"displayname", "Finance Review"}},
	}
	for in, want := range cases {
		got, err := parseFilter(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%q: got %v want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: clause %d got %v want %v", in, i, got[i], want[i])
			}
		}
	}
	for _, bad := range []string{`userName co "a"`, `userName eq`, `userName gt "a" or x eq "b"`} {
		if _, err := parseFilter(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestUserPatchForms(t *testing.T) {
	base := func() userInput {
		return userInput{Email: "old@acme.test", DisplayName: "Old Name", ExternalID: "e1", Active: true}
	}
	// Entra: capitalised op, "False" as a string, no path.
	u := base()
	body := `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Replace","value":{"active":"False"}}]}`
	if err := applyUserPatch([]byte(body), &u); err != nil || u.Active || !u.activeSet {
		t.Fatalf("entra active: %v %+v", err, u)
	}
	// Okta: e-mail through the filtered path, name parts separately.
	u = base()
	body = `{"Operations":[
		{"op":"replace","path":"emails[type eq \"work\"].value","value":"new@acme.test"},
		{"op":"replace","path":"name.givenName","value":"New"},
		{"op":"replace","path":"name.familyName","value":"Person"},
		{"op":"remove","path":"externalId"}]}`
	if err := applyUserPatch([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	if u.Email != "new@acme.test" || u.DisplayName != "New Person" || u.ExternalID != "" {
		t.Fatalf("okta patch: %+v", u)
	}
	// Attributes the platform does not model are accepted and ignored.
	u = base()
	body = `{"Operations":[{"op":"replace","value":{"title":"CFO","userType":"Employee","name":{"formatted":"Only Formatted"}}}]}`
	if err := applyUserPatch([]byte(body), &u); err != nil || u.DisplayName != "Only Formatted" {
		t.Fatalf("ignored attrs: %v %+v", err, u)
	}
	// An unknown path is an error the directory can read.
	u = base()
	body = `{"Operations":[{"op":"replace","path":"favouriteColour","value":"blue"}]}`
	err := applyUserPatch([]byte(body), &u)
	var se *scimErr
	if err == nil || !asScimErr(err, &se) || se.status != 400 || se.typ != "invalidPath" {
		t.Fatalf("unknown path: %v", err)
	}
}

func asScimErr(err error, target **scimErr) bool {
	se, ok := err.(*scimErr)
	if ok {
		*target = se
	}
	return ok
}

func TestDecodeUserFallsBackToPrimaryEmail(t *testing.T) {
	body := `{"userName":"jdoe","name":{"givenName":"Jane","familyName":"Doe"},"emails":[{"value":"JANE@Acme.test","primary":true}]}`
	u, err := decodeUser([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "jane@acme.test" || u.DisplayName != "Jane Doe" || !u.Active {
		t.Fatalf("%+v", u)
	}
	var probe map[string]any
	if json.Unmarshal([]byte(body), &probe) != nil {
		t.Fatal("test body is not JSON")
	}
}
