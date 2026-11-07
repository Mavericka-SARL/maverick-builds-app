package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Access rules a business role carries (migration 156; owner request,
// 2026-11-07): every member of the role holds them as their own, joining or
// leaving the role takes effect at once, and where a role's rule and a
// person's land on one member or metric the stricter wins.

// roleRuleRow is one rule as the Access Rules editor reads it.
type roleRuleRow struct {
	RoleID   string `json:"role_id,omitempty"`
	RoleName string `json:"role_name,omitempty"`
	RuleType string `json:"rule_type"`
	RefID    string `json:"ref_id"`
	RefName  string `json:"ref_name"`
	Access   string `json:"access"`
}

// baRoleAccessRules handles GET and PUT
// /api/business-admin/roles/{id}/access-rules, a role of the workspace the
// business admin administers.
func (h *handler) baRoleAccessRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	roleID := r.PathValue("id")
	wsID, modelID, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}
	var roleWS string
	err = h.db.QueryRow(ctx, `SELECT workspace_id::text FROM identity.business_role WHERE id::text = $1`, roleID).Scan(&roleWS)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && roleWS != wsID) {
		jsonErr(w, fmt.Errorf("role not found"), http.StatusNotFound)
		return
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	if r.Method == http.MethodGet {
		rows, err := h.db.Query(ctx, `
			SELECT rule_type, ref_id, access FROM identity.business_role_access_rule
			WHERE role_id = $1::uuid ORDER BY rule_type, ref_id`, roleID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []roleRuleRow{}
		for rows.Next() {
			var rr roleRuleRow
			if err := rows.Scan(&rr.RuleType, &rr.RefID, &rr.Access); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			out = append(out, rr)
		}
		jsonOK(w, out)
		return
	}

	var body struct {
		Rules []struct {
			RuleType string `json:"rule_type"`
			RefID    string `json:"ref_id"`
			Access   string `json:"access"`
		} `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	inputs := make([]writeguard.RuleInput, 0, len(body.Rules))
	listed := map[[2]string]bool{}
	for _, rule := range body.Rules {
		inputs = append(inputs, writeguard.RuleInput{Type: rule.RuleType, RefID: rule.RefID, Access: rule.Access})
		listed[[2]string{rule.RuleType, rule.RefID}] = true
	}
	// As for a person: the list is the role's rules on the model the admin
	// is working in; its rules on other models are kept.
	kept, err := rulesOnOtherModels(ctx, tx, "identity.business_role_access_rule", "role_id", roleID, modelID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	for _, rule := range kept {
		if !listed[[2]string{rule.Type, rule.RefID}] {
			inputs = append(inputs, rule)
		}
	}
	if err := writeguard.ReplaceRoleRules(ctx, tx, roleID, inputs); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if a, e := h.resolveActor(ctx, r); e == nil {
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventRoleAccessRulesUpdated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ResourceType: "business_role", ResourceID: roleID,
			Metadata: map[string]string{"rules": fmt.Sprint(len(inputs))},
		})
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// baUserRoleRules handles GET /api/business-admin/users/{id}/role-access-rules:
// the rules a person holds through the roles of this workspace, which the
// Access Rules editor shows beside their own.
func (h *handler) baUserRoleRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := r.PathValue("id")
	wsID, _, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}
	rows, err := h.db.Query(ctx, `
		SELECT br.id::text, br.name, rr.rule_type, rr.ref_id, rr.access
		FROM identity.business_role_member m
		JOIN identity.business_role br ON br.id = m.role_id AND br.workspace_id = $2::uuid
		JOIN identity.business_role_access_rule rr ON rr.role_id = br.id
		WHERE m.user_id::text = $1
		ORDER BY br.name, rr.rule_type, rr.ref_id`, userID, wsID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []roleRuleRow{}
	for rows.Next() {
		var rr roleRuleRow
		if err := rows.Scan(&rr.RoleID, &rr.RoleName, &rr.RuleType, &rr.RefID, &rr.Access); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		out = append(out, rr)
	}
	jsonOK(w, out)
}
