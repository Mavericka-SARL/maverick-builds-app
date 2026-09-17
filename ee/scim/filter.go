package scim

import (
	"fmt"
	"regexp"
	"strings"
)

// The subset of RFC 7644 filtering the big identity providers actually
// send: equality on one or two attributes joined by "and". Entra ID looks
// users up by userName or externalId and groups by displayName; Okta by
// userName; both use exactly this grammar. Anything else is refused with
// invalidFilter rather than silently matched against everything.

type clause struct{ attr, value string }

// An attribute is a name (URN-prefixed or not), optionally with a value
// filter in brackets and a sub-attribute: userName, externalId,
// urn:ietf:params:scim:schemas:core:2.0:User:userName,
// emails[type eq "work"].value. No bare spaces: "a gt "x" or b" must not
// read as one attribute.
var clauseRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_:.\-]*(?:\[[^\]]*\])?(?:\.[A-Za-z][A-Za-z0-9_]*)?)\s+eq\s+(?:"([^"]*)"|([^"\s]+))$`)

var orRe = regexp.MustCompile(`(?i)\s+or\s+`)
var andRe = regexp.MustCompile(`(?i)\s+and\s+`)

func parseFilter(s string) ([]clause, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if orRe.MatchString(s) {
		return nil, fmt.Errorf("unsupported filter %q — \"or\" is not supported, only attr eq \"value\" joined by and", s)
	}
	var out []clause
	for _, part := range andRe.Split(s, -1) {
		m := clauseRe.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return nil, fmt.Errorf("unsupported filter %q — only attr eq \"value\" (joined by and) is supported", part)
		}
		value := m[2]
		if value == "" {
			value = m[3]
		}
		out = append(out, clause{attr: normalizeAttr(m[1]), value: value})
	}
	return out, nil
}

// normalizeAttr lower-cases and collapses the address forms Entra uses:
// emails[type eq "work"].value and emails.value both mean the e-mail.
func normalizeAttr(a string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	if strings.HasPrefix(a, "emails") {
		return "emails.value"
	}
	if strings.HasPrefix(a, "members") {
		return "members.value"
	}
	a = strings.TrimPrefix(a, "urn:ietf:params:scim:schemas:core:2.0:user:")
	return strings.TrimPrefix(a, "urn:ietf:params:scim:schemas:core:2.0:group:")
}
