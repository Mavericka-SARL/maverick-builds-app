package assignee

import (
	"errors"
	"fmt"
	"strings"
)

// platformRoleNames are identity.user_role's values. A step or notification
// naming one means that platform role, whatever business role has the name.
var platformRoleNames = []string{"platform_admin", "tenant_admin", "developer", "business_admin", "business_user"}

// ErrReservedRoleName refuses a business role named like a platform role.
var ErrReservedRoleName = errors.New("a business role cannot be named like a platform role")

// CheckBusinessRoleName refuses a business role name that is a platform
// role's, ignoring case and surrounding spaces (owner's decision,
// 2026-10-06; migration 164 holds the same rule in the database). Steps
// assigned to such a role went to the platform's people and never to its
// own members, with no message. Every path that creates or renames a
// business role calls it: Business Admin › Roles, the AI Developer and
// SCIM groups.
func CheckBusinessRoleName(name string) error {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, r := range platformRoleNames {
		if n == r {
			return fmt.Errorf("%w (%s): choose another name", ErrReservedRoleName, r)
		}
	}
	return nil
}
