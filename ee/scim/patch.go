package scim

import (
	"context"
	"encoding/json"
	"strings"
)

// PATCH per RFC 7644 §3.5.2, in the forms the big providers send. Entra ID
// capitalises the op ("Replace"), sends "active" as the string "False", and
// sometimes omits the path with an object value; Okta addresses e-mail as
// emails[type eq "work"].value. Every form resolves to the same update.

type patchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

func decodePatch(body []byte) ([]patchOp, error) {
	var in struct {
		Operations []patchOp `json:"Operations"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, bad("invalidSyntax", "body is not a SCIM PatchOp: "+err.Error())
	}
	if len(in.Operations) == 0 {
		return nil, bad("invalidValue", "Operations is empty")
	}
	for i := range in.Operations {
		in.Operations[i].Op = strings.ToLower(in.Operations[i].Op)
		in.Operations[i].Path = strings.TrimSpace(in.Operations[i].Path)
	}
	return in.Operations, nil
}

func asString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

func asBool(raw json.RawMessage) (bool, bool) {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, true
	}
	switch strings.ToLower(strings.Trim(string(raw), `"`)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func applyUserPatch(body []byte, u *userInput) error {
	ops, err := decodePatch(body)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Path == "" {
			// {"op":"replace","value":{"active":false,"name":{...}}}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(op.Value, &fields); err != nil {
				return bad("invalidValue", "a patch without a path needs an object value")
			}
			for k, v := range fields {
				if err := setUserField(u, normalizeAttr(k), v, op.Op); err != nil {
					return err
				}
			}
			continue
		}
		if err := setUserField(u, normalizeAttr(op.Path), op.Value, op.Op); err != nil {
			return err
		}
	}
	return nil
}

func setUserField(u *userInput, path string, v json.RawMessage, op string) error {
	switch path {
	case "active":
		b, ok := asBool(v)
		if !ok {
			return bad("invalidValue", "active must be a boolean")
		}
		u.Active, u.activeSet = b, true
	case "username", "emails.value":
		if op != "remove" {
			u.Email = strings.ToLower(strings.TrimSpace(asString(v)))
		}
	case "emails":
		var emails []scimEmail
		if json.Unmarshal(v, &emails) == nil {
			for _, e := range emails {
				if e.Primary || u.Email == "" {
					u.Email = strings.ToLower(strings.TrimSpace(e.Value))
				}
			}
		}
	case "externalid":
		if op == "remove" {
			u.ExternalID = ""
		} else {
			u.ExternalID = asString(v)
		}
	case "displayname", "name.formatted":
		if op != "remove" {
			u.DisplayName = strings.TrimSpace(asString(v))
		}
	case "name.givenname":
		_, last := splitName(u.DisplayName)
		u.DisplayName = strings.TrimSpace(asString(v) + " " + last)
	case "name.familyname":
		first, _ := splitName(u.DisplayName)
		u.DisplayName = strings.TrimSpace(first + " " + asString(v))
	case "name":
		var n scimName
		if json.Unmarshal(v, &n) == nil {
			first, last := splitName(u.DisplayName)
			if n.GivenName != "" {
				first = n.GivenName
			}
			if n.FamilyName != "" {
				last = n.FamilyName
			}
			u.DisplayName = strings.TrimSpace(first + " " + last)
			if n.Formatted != "" && n.GivenName == "" && n.FamilyName == "" {
				u.DisplayName = n.Formatted
			}
		}
	case "schemas", "id", "meta", "groups", "title", "usertype", "nickname", "locale", "timezone", "preferredlanguage", "phonenumbers", "addresses":
		// Accepted and ignored: attributes the platform does not model.
	default:
		return bad("invalidPath", "unsupported attribute: "+path)
	}
	return nil
}

func (s *Service) applyGroupPatch(ctx context.Context, g groupRow, body []byte) error {
	ops, err := decodePatch(body)
	if err != nil {
		return err
	}
	for _, op := range ops {
		path := normalizeAttr(op.Path)
		switch {
		case path == "" && op.Op != "remove":
			var fields struct {
				DisplayName string    `json:"displayName"`
				ExternalID  *string   `json:"externalId"`
				Members     []scimRef `json:"members"`
			}
			if err := json.Unmarshal(op.Value, &fields); err != nil {
				return bad("invalidValue", "a patch without a path needs an object value")
			}
			if fields.DisplayName != "" || fields.ExternalID != nil {
				if err := s.renameGroup(ctx, g.ID, fields.DisplayName, fields.ExternalID); err != nil {
					return err
				}
			}
			for _, m := range fields.Members {
				if err := s.addMember(ctx, g.ID, m.Value); err != nil {
					return err
				}
			}
		case path == "displayname":
			if err := s.renameGroup(ctx, g.ID, asString(op.Value), nil); err != nil {
				return err
			}
		case path == "externalid":
			ext := asString(op.Value)
			if op.Op == "remove" {
				ext = ""
			}
			if err := s.renameGroup(ctx, g.ID, "", &ext); err != nil {
				return err
			}
		case strings.HasPrefix(path, "members"):
			// members[value eq "id"] names one member; a bare "members" carries a list.
			if op.Op == "remove" {
				if i := strings.Index(op.Path, `value eq "`); i >= 0 {
					id := op.Path[i+len(`value eq "`):]
					if j := strings.Index(id, `"`); j >= 0 {
						id = id[:j]
					}
					if err := s.removeMember(ctx, g.ID, id); err != nil {
						return err
					}
					continue
				}
				var refs []scimRef
				if json.Unmarshal(op.Value, &refs) == nil && len(refs) > 0 {
					for _, m := range refs {
						if err := s.removeMember(ctx, g.ID, m.Value); err != nil {
							return err
						}
					}
					continue
				}
				if err := s.setMembers(ctx, g.ID, nil); err != nil {
					return err
				}
				continue
			}
			var refs []scimRef
			if err := json.Unmarshal(op.Value, &refs); err != nil {
				var one scimRef
				if json.Unmarshal(op.Value, &one) != nil || one.Value == "" {
					return bad("invalidValue", "members must be a list of {value: userId}")
				}
				refs = []scimRef{one}
			}
			if op.Op == "replace" {
				if err := s.setMembers(ctx, g.ID, nil); err != nil {
					return err
				}
			}
			for _, m := range refs {
				if err := s.addMember(ctx, g.ID, m.Value); err != nil {
					return err
				}
			}
		default:
			return bad("invalidPath", "unsupported group attribute: "+op.Path)
		}
	}
	return nil
}

func (s *Service) renameGroup(ctx context.Context, id, name string, externalID *string) error {
	if name != "" {
		if _, err := s.Pool.Exec(ctx, `UPDATE identity.business_role SET name = $2 WHERE id = $1::uuid`, id, strings.TrimSpace(name)); err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
				return conflict("a group named " + name + " already exists")
			}
			return internal(err)
		}
	}
	if externalID != nil {
		if _, err := s.Pool.Exec(ctx, `UPDATE identity.business_role SET external_id = NULLIF($2,'') WHERE id = $1::uuid`, id, *externalID); err != nil {
			return internal(err)
		}
	}
	return nil
}
