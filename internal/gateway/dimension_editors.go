package gateway

// Who may change a dimension's members and properties outside the developer's
// tools (migration 161, owner decision 2026-11-08): a dimension's
// editor_roles names the business roles that may add, change and delete its
// members — through the business member editor and, for a dimension a table
// of records keeps, through that table's records. None named: as before.
// The application's administrators and the model's developers always may.
// A GL account's reporting category, kept as a member property, could be
// changed by any business user; the workbook's Mapping is Accounting's.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/workflow/assignee"
)

// dimensionEditorRefusal says why act may not change dimID's members (""
// when they may).
func (h *handler) dimensionEditorRefusal(ctx context.Context, act *actor, dimID string) (string, error) {
	var name, modelID, appID string
	var roles []string
	if err := h.db.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(d.label,''), d.name), d.model_id::text, m.application_id::text, d.editor_roles
		FROM model.dimension_def d JOIN core.model m ON m.id = d.model_id
		WHERE d.id = $1::uuid`, dimID).Scan(&name, &modelID, &appID, &roles); err != nil {
		return "", fmt.Errorf("resolve the dimension's editors: %w", err)
	}
	if len(roles) == 0 {
		return "", nil
	}
	admin, err := h.administersApp(ctx, act, modelID, appID)
	if err != nil || admin {
		return "", err
	}
	rolesJSON, _ := json.Marshal(roles)
	var holds bool
	if err := h.db.QueryRow(ctx, `SELECT `+assignee.SQL("$1::uuid", "$2::jsonb", "$3::uuid"), appID, string(rolesJSON), act.UserID).Scan(&holds); err != nil {
		return "", fmt.Errorf("resolve the dimension's editors: %w", err)
	}
	if holds {
		return "", nil
	}
	return fmt.Sprintf("only %s and the application's administrators change the members of %s", strings.Join(roles, ", "), name), nil
}
