package aiassistant_test

// set_user_access_rules — the first AI tool that deliberately reaches beyond
// the developer role (a business-admin capability, extended to the AI
// Developer at the owner's direction, 2026-08-26). Everything resolves by
// NAME (email, dimension, member code): a language model cannot be trusted
// to transcribe UUIDs. The replace-all semantics mirror the business-admin
// console's PUT /access-rules.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

type accessRulesFixture struct {
	pool               *pgxpool.Pool
	modelID, revID     string
	teamID, outsiderID string
	exec               *aiassistant.WriteExecutor
}

func setupAccessRulesFixture(t *testing.T) *accessRulesFixture {
	t.Helper()
	ctx := context.Background()
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	actorID := seedActor(t, pool)

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	var wsID string
	if err := pool.QueryRow(ctx, `
		SELECT a.workspace_id::text FROM core.application a
		JOIN core.model m ON m.application_id = a.id WHERE m.id=$1::uuid
	`, modelID).Scan(&wsID); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}

	teamID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('team-q', 'team@example.com', 'Team q') RETURNING id::text`)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, teamID, wsID); err != nil {
		t.Fatalf("assign role: %v", err)
	}

	// A same-email user in a DIFFERENT customer must never be reachable.
	otherCust := q(`INSERT INTO core.customer (name) VALUES ('Other Corp') RETURNING id::text`)
	otherWs := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Other WS') RETURNING id::text`, otherCust)
	outsiderID := q(`INSERT INTO identity.user (keycloak_sub, email) VALUES ('outsider', 'outsider@example.com') RETURNING id::text`)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, outsiderID, otherWs); err != nil {
		t.Fatalf("assign outsider role: %v", err)
	}

	exec := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, actorID)
	f := &accessRulesFixture{pool: pool, modelID: modelID, revID: revID, teamID: teamID, outsiderID: outsiderID, exec: exec}

	// geography with a small hierarchy in the ACTIVE revision.
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$2::uuid WHERE id=$1::uuid`, modelID, revID); err != nil {
		t.Fatalf("set active revision: %v", err)
	}
	dimID := q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'geography', $2::uuid) RETURNING id::text`, modelID, revID)
	for _, code := range []string{"CA", "US", "UK"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $2) RETURNING id::text`, dimID, code)
	}
	return f
}

func (f *accessRulesFixture) rules(t *testing.T, userID string) map[string]string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT m.code, r.access FROM identity.user_access_rule r
		JOIN model.dimension_member m ON m.id::text = r.ref_id
		WHERE r.user_id=$1::uuid
	`, userID)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var code, access string
		_ = rows.Scan(&code, &access)
		out[code] = access
	}
	return out
}

func TestSetUserAccessRules_HidesEverythingButTheAllowedMember(t *testing.T) {
	f := setupAccessRulesFixture(t)
	ctx := context.Background()

	result, _, err := f.exec.Execute(ctx, "set_user_access_rules", mustJSON(t, map[string]any{
		"user_email": "team@example.com",
		"rules": []map[string]string{
			{"dimension": "geography", "member_code": "US", "access": "hidden"},
			{"dimension": "geography", "member_code": "UK", "access": "hidden"},
		},
	}))
	if err != nil {
		t.Fatalf("set_user_access_rules: %v", err)
	}
	if !strings.Contains(result, "team@example.com") {
		t.Errorf("result should name the user, got %q", result)
	}
	got := f.rules(t, f.teamID)
	if len(got) != 2 || got["US"] != "hidden" || got["UK"] != "hidden" {
		t.Fatalf("rules = %v, want US/UK hidden and CA unrestricted", got)
	}

	// Replace-all: a second call with one rule leaves exactly one rule.
	if _, _, err := f.exec.Execute(ctx, "set_user_access_rules", mustJSON(t, map[string]any{
		"user_email": "team@example.com",
		"rules":      []map[string]string{{"dimension": "geography", "member_code": "US", "access": "read"}},
	})); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got = f.rules(t, f.teamID)
	if len(got) != 1 || got["US"] != "read" {
		t.Fatalf("after replace rules = %v, want only US=read", got)
	}

	// The same audit event the business-admin endpoint writes.
	var audits int
	_ = f.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit.audit_event
		WHERE event_type='user.access_rules_updated' AND resource_id=$1
	`, f.teamID).Scan(&audits)
	if audits != 2 {
		t.Errorf("audit events = %d, want 2 (one per replace)", audits)
	}
}

func TestSetUserAccessRules_FailsClosed(t *testing.T) {
	f := setupAccessRulesFixture(t)
	ctx := context.Background()

	// A user outside this application's customer is unreachable.
	if _, _, err := f.exec.Execute(ctx, "set_user_access_rules", mustJSON(t, map[string]any{
		"user_email": "outsider@example.com",
		"rules":      []map[string]string{{"dimension": "geography", "member_code": "US", "access": "hidden"}},
	})); err == nil {
		t.Fatal("expected refusal for a user outside the application's workspaces")
	}

	// An unknown member code fails before anything is written.
	if _, _, err := f.exec.Execute(ctx, "set_user_access_rules", mustJSON(t, map[string]any{
		"user_email": "team@example.com",
		"rules":      []map[string]string{{"dimension": "geography", "member_code": "MARS", "access": "hidden"}},
	})); err == nil || !strings.Contains(err.Error(), "MARS") {
		t.Fatalf("expected unknown-member error naming MARS, got %v", err)
	}
	if got := f.rules(t, f.teamID); len(got) != 0 {
		t.Fatalf("failed call must write nothing, got %v", got)
	}

	// Only read/hidden are valid levels.
	if _, _, err := f.exec.Execute(ctx, "set_user_access_rules", mustJSON(t, map[string]any{
		"user_email": "team@example.com",
		"rules":      []map[string]string{{"dimension": "geography", "member_code": "US", "access": "banish"}},
	})); err == nil {
		t.Fatal("expected rejection of an invalid access level")
	}
}
