// Package auditlog is the single write path for audit.audit_event, used by
// internal/gateway (HTTP handlers) and internal/workflow (the scheduler,
// which has no HTTP-request-scoped actor). It replaces two prior
// independent, ad hoc INSERT call sites with one shared helper and a fixed
// vocabulary of event types.
package auditlog

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

type Category string

const (
	CategoryAuth         Category = "auth"
	CategoryDataChange   Category = "data_change"
	CategoryModelChange  Category = "model_change"
	CategoryPolicyChange Category = "policy_change"
	CategoryAdmin        Category = "admin"
	CategoryAIAssistant  Category = "ai_assistant"
)

type EventType string

// Naming convention: "<resource>.<verb_past_tense>[_<qualifier>]". This
// block is the fixed vocabulary of event types — no inline string literals
// at call sites.
const (
	// Pre-existing, carried over unchanged from the old per-handler
	// logAudit call sites (same string values, so replacing those call
	// sites with auditlog.Log is behavior-preserving).
	EventModelExported          EventType = "model.exported"
	EventModelImported          EventType = "model.imported"
	EventRevisionCreated        EventType = "revision.created"
	EventRevisionActivated      EventType = "revision.activated"
	EventRevisionDeleted        EventType = "revision.deleted"
	EventCellWritten            EventType = "cell.written"
	EventTaskCompleted          EventType = "task.completed"
	EventWorkflowSubmitted      EventType = "workflow.submitted"
	EventMetricCreated          EventType = "metric.created"
	EventDimensionCreated       EventType = "dimension.created"
	EventWidgetCreated          EventType = "widget.created"
	EventRoleCreated            EventType = "role.created"
	EventRoleUpdated            EventType = "role.updated"
	EventRoleDeleted            EventType = "role.deleted"
	EventUserAccessRulesUpdated EventType = "user.access_rules_updated"

	EventAutomationRuleScheduledMisfireSkipped EventType = "automation_rule.scheduled_misfire_skipped"
	EventAutomationRuleScheduledFireFailed     EventType = "automation_rule.scheduled_fire_failed"
	EventAutomationRuleScheduledFire           EventType = "automation_rule.scheduled_fire"

	// Batch 1 — Platform Admin (/api/admin/...). The single biggest
	// pre-existing coverage gap: tenant/application/model/revision/user
	// management was entirely unlogged.
	EventTenantCreated EventType = "tenant.created"
	EventTenantUpdated EventType = "tenant.updated"
	// A tenant that created itself through public sign-up, and a change
	// to the plan catalog (internal/plan).
	EventTenantSignedUp EventType = "tenant.signed_up"
	EventPlanUpdated    EventType = "plan.updated"
	// Outbound notification delivery: which channels a tenant sends on, and
	// an administrator proving the relay works by mailing themselves.
	EventNotificationSettingsUpdated EventType = "notification.settings_updated"
	EventNotificationTestSent        EventType = "notification.test_sent"
	EventTenantDeleted               EventType = "tenant.deleted"
	EventApplicationCreated          EventType = "application.created"
	EventApplicationUpdated          EventType = "application.updated"
	EventApplicationDeleted          EventType = "application.deleted"
	EventModelCreated                EventType = "model.created"
	// EventModelActiveRevisionSet is deliberately distinct from
	// EventRevisionActivated (the developer-console path): both flip a
	// model's active revision, but through different UIs/roles, and should
	// stay independently auditable rather than collapsing into one event.
	EventModelActiveRevisionSet EventType = "model.active_revision_set"
	EventModelDeleted           EventType = "model.deleted"
	// EventRevisionUpdated is new — the developer-console revision path has
	// no PATCH today, only admin's does.
	EventRevisionUpdated        EventType = "revision.updated"
	EventUserCreated            EventType = "user.created"
	EventUserUpdated            EventType = "user.updated"
	EventUserDeleted            EventType = "user.deleted"
	EventUserRoleGranted        EventType = "user.role_granted"
	EventUserRoleRevoked        EventType = "user.role_revoked"
	EventUserAppAccessGranted   EventType = "user.app_access_granted"
	EventUserAppAccessRevoked   EventType = "user.app_access_revoked"
	EventUserModelAccessGranted EventType = "user.model_access_granted"
	EventUserModelAccessRevoked EventType = "user.model_access_revoked"

	// Batch 2 — Business Admin (/api/business-admin/roles/...). role.created/
	// updated/deleted already existed; these close the remaining gaps in the
	// same handler family.
	EventRoleDashboardsUpdated EventType = "role.dashboards_updated"
	EventRoleMemberAdded       EventType = "role.member_added"
	EventRoleMemberRemoved     EventType = "role.member_removed"

	// Batch 3 — Developer Console, model-authoring surfaces
	// (/api/developer/{metrics,dimensions,grids,folders,dashboards}/...).
	// Dimension member/property sub-action edits fold into
	// EventDimensionUpdated with a metadata discriminator rather than
	// getting their own constants — they're not independently
	// security-sensitive the way e.g. workflow publish/archive is.
	EventMetricUpdated    EventType = "metric.updated"
	EventMetricDeleted    EventType = "metric.deleted"
	EventDimensionUpdated EventType = "dimension.updated"
	EventDimensionDeleted EventType = "dimension.deleted"
	EventGridCreated      EventType = "grid.created"
	EventGridUpdated      EventType = "grid.updated"
	EventGridDeleted      EventType = "grid.deleted"
	EventFolderCreated    EventType = "folder.created"
	EventFolderUpdated    EventType = "folder.updated"
	EventFolderDeleted    EventType = "folder.deleted"
	EventDashboardCreated EventType = "dashboard.created"
	EventDashboardUpdated EventType = "dashboard.updated"
	EventDashboardDeleted EventType = "dashboard.deleted"
	EventWidgetUpdated    EventType = "widget.updated"
	EventWidgetDeleted    EventType = "widget.deleted"

	// Batch 4 — Developer Console, workflow-authoring + integration +
	// migration surfaces. EventWorkflowPublished/Archived are deliberately
	// distinct from EventWorkflowDefUpdated — publish/archive are the
	// security-sensitive "revision promotion" case the backlog item calls
	// out by name, not routine edits.
	EventWorkflowDefCreated        EventType = "workflow_def.created"
	EventWorkflowDefUpdated        EventType = "workflow_def.updated"
	EventWorkflowDefDeleted        EventType = "workflow_def.deleted"
	EventWorkflowPublished         EventType = "workflow_def.published"
	EventWorkflowArchived          EventType = "workflow_def.archived"
	EventIntegrationCreated        EventType = "integration.created"
	EventIntegrationUpdated        EventType = "integration.updated"
	EventIntegrationDeleted        EventType = "integration.deleted"
	EventIntegrationRun            EventType = "integration.run"
	EventFormIntegrationCreated    EventType = "form_integration.created"
	EventFormIntegrationUpdated    EventType = "form_integration.updated"
	EventFormIntegrationDeleted    EventType = "form_integration.deleted"
	EventFormIntegrationBackfilled EventType = "form_integration.backfilled"
	EventSchemaMigrationGenerated  EventType = "schema_migration.generated"
	EventSchemaMigrationApplied    EventType = "schema_migration.applied"

	// Batch 5 — AI Assistant (/api/ai/...), first real use of
	// CategoryAIAssistant. Audited at session/document/proposal granularity,
	// not per chat message — logging every turn would flood the 200-row-
	// capped admin audit view with conversational noise without adding a
	// security-relevant signal; session lifecycle, document access, and
	// proposal confirm/reject (actual model mutations) are the
	// security-sensitive decisions here. EventAIProposalConfirmed is kept
	// distinct from EventRevisionActivated (aiPromoteDraft) — "confirmed a
	// proposal" and "activated a revision" are two separate actions even
	// when they happen in the same user flow.
	EventAISessionDeleted    EventType = "ai_session.deleted"
	EventAIDocumentUploaded  EventType = "ai_document.uploaded"
	EventAIDocumentDeleted   EventType = "ai_document.deleted"
	EventAIProposalConfirmed EventType = "ai_proposal.confirmed"
	EventAIProposalRejected  EventType = "ai_proposal.rejected"
	EventAISettingsUpdated   EventType = "ai_settings.updated"
	EventAISettingsTested    EventType = "ai_settings.tested"
	// Tenant-level AI key (enterprise). Separate from the per-user events
	// above: one names a developer changing their own key, these name a
	// tenant admin changing the key every developer in the tenant then uses.
	EventTenantAISettingsUpdated EventType = "ai_settings.tenant_updated"
	EventTenantAIKeyCleared      EventType = "ai_settings.tenant_cleared"
	// A tenant's own credential for an external system (core.tenant_credential):
	// stored, tested, or removed.
	EventTenantCredentialUpdated EventType = "tenant_credential.updated"
	EventTenantCredentialTested  EventType = "tenant_credential.tested"
	EventTenantCredentialDeleted EventType = "tenant_credential.deleted"
	// Enterprise identity: a tenant's own sign-in provider and the tokens
	// that let its directory provision users.
	EventSSOProviderUpdated EventType = "sso.provider_updated"
	EventSSOProviderRemoved EventType = "sso.provider_removed"
	EventSSOUserProvisioned EventType = "sso.user_provisioned"
	EventScimTokenIssued    EventType = "scim.token_issued"
	EventScimTokenRevoked   EventType = "scim.token_revoked"
	// The audit log's own events: an export is itself something to answer
	// for, and so is changing how long the record is kept.
	EventAuditExported         EventType = "audit.exported"
	EventAuditRetentionUpdated EventType = "audit.retention_updated"
	// White-labelling: the tenant's own look on the console and its mail.
	EventBrandingUpdated EventType = "branding.updated"
	EventBrandingRemoved EventType = "branding.removed"

	// Batch 6 — Workflow runtime (/api/workflow/instances/...).
	// EventWorkflowInstanceStarted covers direct starts; EventWorkflowSubmitted
	// (pre-existing) already covers form-submission-triggered starts.
	EventWorkflowInstanceStarted          EventType = "workflow_instance.started"
	EventWorkflowInstanceStatusOverridden EventType = "workflow_instance.status_overridden"

	// Batch 7 — Forms / CRUD app (/api/forms/..., /api/records/...).
	EventFormCreated       EventType = "form.created"
	EventFormUpdated       EventType = "form.updated"
	EventFormDeleted       EventType = "form.deleted"
	EventFormImported      EventType = "form.imported"
	EventFormSynced        EventType = "form.synced"
	EventFormRecordCreated EventType = "form_record.created"
	EventRecordUpdated     EventType = "form_record.updated"
	EventRecordDeleted     EventType = "form_record.deleted"

	// Batch 8 — Automation (/api/automation/...). EventAutomationRuleTriggeredManually
	// is deliberately distinct from the scheduler's own
	// EventAutomationRuleScheduledFire, so manual vs. scheduled firing stay
	// independently auditable.
	EventAutomationRuleCreated           EventType = "automation_rule.created"
	EventAutomationRuleUpdated           EventType = "automation_rule.updated"
	EventAutomationRuleDeleted           EventType = "automation_rule.deleted"
	EventAutomationRuleTriggeredManually EventType = "automation_rule.triggered_manually"

	// Batch 9 — Import (/api/import/...). Closes the last audit-coverage
	// gap identified in the original mutation inventory.
	EventImportUploaded   EventType = "import.uploaded"
	EventImportJobDeleted EventType = "import.job_deleted"
)

// Fields is one audit event. ActorUserID, ActorRole, ApplicationID, and
// RevisionID are all optional ("" -> NULL / not attributed); which ones are
// meaningful depends on the event — e.g. a scheduler-fired event has no
// actor, and a tenant/user-management event has no revision.
type Fields struct {
	Category      Category
	EventType     EventType
	ActorUserID   string
	ActorRole     string
	ApplicationID string
	ResourceType  string
	ResourceID    string
	RevisionID    string
	Metadata      map[string]string
}

// nullableArgs isolates the ""->nil conversion as a pure function so it's
// unit-testable without a database.
func nullableArgs(f Fields) (actor, application, revision *string) {
	if f.ActorUserID != "" {
		actor = &f.ActorUserID
	}
	if f.ApplicationID != "" {
		application = &f.ApplicationID
	}
	if f.RevisionID != "" {
		revision = &f.RevisionID
	}
	return actor, application, revision
}

// Log writes one audit.audit_event row synchronously in the caller's
// goroutine — every prior call site behaved this way, and tests assert on
// the row immediately after the triggering call returns. A write failure is
// logged (unlike the old logAudit, which silently swallowed it) but never
// returned: audit logging must never fail the primary response.
func Log(ctx context.Context, pool *pgxpool.Pool, log zerolog.Logger, f Fields) {
	if f.Metadata == nil {
		// A nil map marshals to the JSON scalar null; every reader expects an
		// object, and exports render null as the word "null".
		f.Metadata = map[string]string{}
	}
	metaJSON, err := json.Marshal(f.Metadata)
	if err != nil {
		log.Warn().Err(err).Str("event_type", string(f.EventType)).Msg("audit log metadata marshal failed")
		return
	}
	actorArg, appArg, revArg := nullableArgs(f)
	_, err = pool.Exec(ctx, `
		INSERT INTO audit.audit_event
		    (category, event_type, actor_user_id, actor_role, application_id,
		     resource_type, resource_id, metadata, revision_id)
		VALUES ($9, $1, $2::uuid, $3, $4::uuid, $5, $6, $7, $8::uuid)
	`, string(f.EventType), actorArg, f.ActorRole, appArg, f.ResourceType, f.ResourceID, metaJSON, revArg, string(f.Category))
	if err != nil {
		log.Warn().Err(err).Str("event_type", string(f.EventType)).Str("resource_id", f.ResourceID).Msg("audit log write failed")
	}
}
