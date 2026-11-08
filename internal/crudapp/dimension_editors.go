package crudapp

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// CheckEditorRoles refuses names in roles that are no business role of the
// customer dimension dimID's model belongs to, naming the ones there are —
// a dimension's editor_roles (migration 161), set by the developer API and
// the AI Developer alike.
func CheckEditorRoles(ctx context.Context, db dbx.DB, dimID string, roles []string) error {
	if len(roles) == 0 {
		return nil
	}
	var known []string
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(DISTINCT br.name ORDER BY br.name), '{}')
		FROM identity.business_role br
		JOIN core.workspace w ON w.id = br.workspace_id
		WHERE w.customer_id = (
		    SELECT COALESCE(a.customer_id, ws.customer_id)
		    FROM model.dimension_def d
		    JOIN core.model mo ON mo.id = d.model_id
		    JOIN core.application a ON a.id = mo.application_id
		    LEFT JOIN core.workspace ws ON ws.id = a.workspace_id
		    WHERE d.id = $1::uuid)`, dimID).Scan(&known); err != nil {
		return err
	}
	for _, r := range roles {
		if !slices.Contains(known, r) {
			return &MappingRefError{msg: fmt.Sprintf("editor_roles: %q is no business role here (there are: %s)", r, strings.Join(known, ", "))}
		}
	}
	return nil
}
