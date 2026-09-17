package policy

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	policyv1 "github.com/mavericks-engine/mavericks/gen/go/policy/v1"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// GetUserRole returns the highest-priority role for the given user in the given workspace.
// Platform-admin and developer roles are customer-level (workspace_id IS NULL).
func (s *Store) GetUserRole(ctx context.Context, userID, workspaceID string) (commonv1.Role, error) {
	// Try workspace-scoped role first, then customer-level
	var roleStr string
	err := s.pool.QueryRow(ctx, `
		SELECT role FROM identity.role_assignment
		WHERE user_id = $1
		  AND (workspace_id = $2 OR workspace_id IS NULL)
		ORDER BY
		  CASE role
		    WHEN 'platform_admin' THEN 1
		    WHEN 'developer'      THEN 2
		    WHEN 'business_admin' THEN 3
		    WHEN 'business_user'  THEN 4
		    ELSE 5
		  END
		LIMIT 1
	`, userID, workspaceID).Scan(&roleStr)
	if err != nil {
		return commonv1.Role_ROLE_UNSPECIFIED, err
	}
	return roleFromString(roleStr), nil
}

// GetRACIType returns the RACI type for a user on a resource pattern within an application.
func (s *Store) GetRACIType(ctx context.Context, userID, applicationID, resourcePattern string) (policyv1.RACIType, error) {
	var raciStr string
	err := s.pool.QueryRow(ctx, `
		SELECT raci_type FROM security.raci_rule
		WHERE user_id = $1
		  AND application_id = $2
		  AND resource_pattern = $3
		LIMIT 1
	`, userID, applicationID, resourcePattern).Scan(&raciStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return policyv1.RACIType_RACI_TYPE_UNSPECIFIED, nil
	}
	if err != nil {
		return policyv1.RACIType_RACI_TYPE_UNSPECIFIED, err
	}
	return raciFromString(raciStr), nil
}

// GetAllowedMetrics returns metric IDs the user can read for an application.
// Returns nil (no restriction) for non-business-user roles.
func (s *Store) GetAllowedMetrics(ctx context.Context, userID, applicationID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT metric_id::text FROM security.metric_policy
		WHERE user_id = $1 AND application_id = $2 AND can_read = true
	`, userID, applicationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetAllowedDimensionMembers returns allowed member codes per dimension for a user.
func (s *Store) GetAllowedDimensionMembers(ctx context.Context, userID, applicationID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT unnest(allowed_member_codes) FROM security.dimension_member_policy
		WHERE user_id = $1 AND application_id = $2
	`, userID, applicationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var codes []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, rows.Err()
}

func (s *Store) UpsertRACIRule(ctx context.Context, rule *policyv1.RACIRule) (*policyv1.RACIRule, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO security.raci_rule (application_id, user_id, resource_pattern, raci_type)
		VALUES ($1, $2, $3, $4::security.raci_type)
		ON CONFLICT (application_id, user_id, resource_pattern, raci_type)
		DO UPDATE SET raci_type = EXCLUDED.raci_type
		RETURNING id
	`, rule.ApplicationId, rule.UserId, rule.ResourcePattern, raciToString(rule.RaciType)).Scan(&id)
	if err != nil {
		return nil, err
	}
	rule.Id = id
	return rule, nil
}

func (s *Store) UpsertDimensionPolicy(ctx context.Context, p *policyv1.DimensionPolicy) (*policyv1.DimensionPolicy, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO security.dimension_member_policy
		    (application_id, user_id, dimension_id, allowed_member_codes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (application_id, user_id, dimension_id)
		DO UPDATE SET allowed_member_codes = EXCLUDED.allowed_member_codes
		RETURNING id
	`, p.ApplicationId, p.UserId, p.DimensionId, p.AllowedMemberCodes).Scan(&id)
	if err != nil {
		return nil, err
	}
	p.Id = id
	return p, nil
}

func (s *Store) UpsertMetricPolicy(ctx context.Context, p *policyv1.MetricPolicy) (*policyv1.MetricPolicy, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO security.metric_policy
		    (application_id, user_id, metric_id, can_read, can_write)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (application_id, user_id, metric_id)
		DO UPDATE SET can_read = EXCLUDED.can_read, can_write = EXCLUDED.can_write
		RETURNING id
	`, p.ApplicationId, p.UserId, p.MetricId, p.CanRead, p.CanWrite).Scan(&id)
	if err != nil {
		return nil, err
	}
	p.Id = id
	return p, nil
}

// ── ABAC ──────────────────────────────────────────────────────────────────────

// UpsertAttributeRule stores an ABAC rule that grants the listed actions on
// resource_type when actor.attribute_name == attribute_value.
func (s *Store) UpsertAttributeRule(ctx context.Context, r *policyv1.AttributeRule) (*policyv1.AttributeRule, error) {
	actionsStr := make([]string, len(r.Actions))
	for i, a := range r.Actions {
		actionsStr[i] = actionToString(a)
	}

	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO security.abac_rule
		    (application_id, name, resource_type, expression, actions)
		VALUES ($1, $2, $3, $4, $5::security.policy_action[])
		ON CONFLICT (application_id, name)
		DO UPDATE SET
		    resource_type = EXCLUDED.resource_type,
		    expression    = EXCLUDED.expression,
		    actions       = EXCLUDED.actions,
		    updated_at    = now()
		RETURNING id
	`,
		r.ApplicationId,
		r.Name,
		r.ResourceType,
		// Encode as "attribute_name == attribute_value" for the expression column
		r.AttributeName+"=="+r.AttributeValue,
		actionsStr,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	r.Id = id
	return r, nil
}

// GetAttributeRules loads all ABAC rules for an application and resource type.
func (s *Store) GetAttributeRules(ctx context.Context, applicationID, resourceType string) ([]*policyv1.AttributeRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, resource_type, expression, actions::text[]
		FROM security.abac_rule
		WHERE application_id = $1 AND resource_type = $2
	`, applicationID, resourceType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []*policyv1.AttributeRule
	for rows.Next() {
		var r policyv1.AttributeRule
		var expr string
		var actionsStr []string
		if err := rows.Scan(&r.Id, &r.Name, &r.ResourceType, &expr, &actionsStr); err != nil {
			return nil, err
		}
		r.ApplicationId = applicationID
		// Parse "attribute_name==attribute_value"
		if idx := len(expr) - 1; idx > 0 {
			for i, c := range expr {
				if c == '=' && i+1 < len(expr) && expr[i+1] == '=' {
					r.AttributeName = expr[:i]
					r.AttributeValue = expr[i+2:]
					break
				}
			}
		}
		for _, a := range actionsStr {
			r.Actions = append(r.Actions, actionFromString(a))
		}
		rules = append(rules, &r)
	}
	return rules, rows.Err()
}

// ── Cell policy ───────────────────────────────────────────────────────────────

func (s *Store) UpsertCellPolicy(ctx context.Context, p *policyv1.CellPolicy) (*policyv1.CellPolicy, error) {
	dimJSON, err := json.Marshal(p.DimMembers)
	if err != nil {
		return nil, err
	}

	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO security.cell_policy
		    (application_id, user_id, metric_id, dim_members, can_read, can_write)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, p.ApplicationId, p.UserId, p.MetricId, dimJSON, p.CanRead, p.CanWrite).Scan(&id)
	if err != nil {
		return nil, err
	}
	p.Id = id
	return p, nil
}

// CheckCellPolicy returns the most-specific cell policy for the given user/metric/dims,
// or (nil, nil) if no policy exists (caller should fall back to metric-level policy).
func (s *Store) CheckCellPolicy(ctx context.Context, userID, applicationID, metricID string, dimMembers map[string]string) (*policyv1.CellPolicy, error) {
	dimJSON, err := json.Marshal(dimMembers)
	if err != nil {
		return nil, err
	}

	var p policyv1.CellPolicy
	err = s.pool.QueryRow(ctx, `
		SELECT id, can_read, can_write FROM security.cell_policy
		WHERE user_id = $1
		  AND application_id = $2
		  AND metric_id = $3
		  AND dim_members @> $4::jsonb
		LIMIT 1
	`, userID, applicationID, metricID, dimJSON).Scan(&p.Id, &p.CanRead, &p.CanWrite)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.UserId = userID
	p.ApplicationId = applicationID
	p.MetricId = metricID
	return &p, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func roleFromString(s string) commonv1.Role {
	switch s {
	case "platform_admin":
		return commonv1.Role_ROLE_PLATFORM_ADMIN
	case "developer":
		return commonv1.Role_ROLE_DEVELOPER
	case "business_admin":
		return commonv1.Role_ROLE_BUSINESS_ADMIN
	case "business_user":
		return commonv1.Role_ROLE_BUSINESS_USER
	case "tenant_admin":
		return commonv1.Role_ROLE_TENANT_ADMIN
	default:
		return commonv1.Role_ROLE_UNSPECIFIED
	}
}

func raciFromString(s string) policyv1.RACIType {
	switch s {
	case "responsible":
		return policyv1.RACIType_RACI_TYPE_RESPONSIBLE
	case "accountable":
		return policyv1.RACIType_RACI_TYPE_ACCOUNTABLE
	case "consulted":
		return policyv1.RACIType_RACI_TYPE_CONSULTED
	case "informed":
		return policyv1.RACIType_RACI_TYPE_INFORMED
	default:
		return policyv1.RACIType_RACI_TYPE_UNSPECIFIED
	}
}

func actionToString(a policyv1.Action) string {
	switch a {
	case policyv1.Action_ACTION_READ:
		return "read"
	case policyv1.Action_ACTION_WRITE:
		return "write"
	case policyv1.Action_ACTION_APPROVE:
		return "approve"
	case policyv1.Action_ACTION_ADMIN:
		return "admin"
	default:
		return "read"
	}
}

func actionFromString(s string) policyv1.Action {
	switch s {
	case "write":
		return policyv1.Action_ACTION_WRITE
	case "approve":
		return policyv1.Action_ACTION_APPROVE
	case "admin":
		return policyv1.Action_ACTION_ADMIN
	default:
		return policyv1.Action_ACTION_READ
	}
}

func raciToString(r policyv1.RACIType) string {
	switch r {
	case policyv1.RACIType_RACI_TYPE_RESPONSIBLE:
		return "responsible"
	case policyv1.RACIType_RACI_TYPE_ACCOUNTABLE:
		return "accountable"
	case policyv1.RACIType_RACI_TYPE_CONSULTED:
		return "consulted"
	case policyv1.RACIType_RACI_TYPE_INFORMED:
		return "informed"
	default:
		return "informed"
	}
}
