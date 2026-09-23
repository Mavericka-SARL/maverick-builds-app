package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Settings is one row of notification.settings: which outbound channels a
// tenant delivers on, and whether its overdue tasks are reminded. Rows are
// keyed by tenant (migration 091); the row with no tenant is the
// deployment's own — what a tenant inherits until it sets its own, on an
// edition that includes deployment settings.
//
// SMTP credentials are not here on purpose. They belong to whoever runs the
// deployment (see Mailer), not to a tenant admin who could otherwise point
// the relay somewhere else.
type Settings struct {
	EmailEnabled   bool   `json:"email_enabled"`
	WebhookEnabled bool   `json:"webhook_enabled"`
	WebhookURL     string `json:"webhook_url"`
	// WebhookSecret is write-only over the API: it is never returned, and an
	// empty value on update keeps the stored one.
	WebhookSecret     string `json:"webhook_secret,omitempty"`
	HasWebhookSecret  bool   `json:"has_webhook_secret"`
	RemindersEnabled  bool   `json:"reminders_enabled"`
	ReminderLeadHours int32  `json:"reminder_lead_hours"`
	// webhookSecretValue is the stored secret, carried inside the process
	// so the dispatcher signs with the scope's own — never serialised.
	webhookSecretValue string
}

// DeploymentScope is the customer id of the deployment's own row.
const DeploymentScope = ""

// GetSettings reads one scope's row: a tenant's by id, the deployment's for
// DeploymentScope. found is false when the scope has never saved anything,
// and the zero Settings (every channel off) come back.
func (s *Store) GetSettings(ctx context.Context, customerID string) (out Settings, found bool, err error) {
	var secret string
	err = s.pool.QueryRow(ctx, `
		SELECT email_enabled, webhook_enabled, webhook_url, webhook_secret,
		       reminders_enabled, reminder_lead_hours
		FROM notification.settings WHERE customer_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid
	`, customerID).Scan(&out.EmailEnabled, &out.WebhookEnabled, &out.WebhookURL, &secret,
		&out.RemindersEnabled, &out.ReminderLeadHours)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, fmt.Errorf("read notification settings: %w", err)
	}
	out.HasWebhookSecret = secret != ""
	out.webhookSecretValue = secret
	return out, true, nil
}

// UpdateSettings writes one scope's row, creating it on first save. An
// empty WebhookSecret keeps the stored one, so a console that never
// receives the secret can still save the form.
func (s *Store) UpdateSettings(ctx context.Context, customerID string, in Settings) (Settings, error) {
	if in.WebhookEnabled {
		u := strings.TrimSpace(in.WebhookURL)
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return Settings{}, fmt.Errorf("webhook url must be an http(s) address")
		}
	}
	if in.ReminderLeadHours < 0 {
		return Settings{}, fmt.Errorf("reminder lead hours cannot be negative")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification.settings
		    (customer_id, email_enabled, webhook_enabled, webhook_url, webhook_secret, reminders_enabled, reminder_lead_hours)
		VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (customer_id) DO UPDATE SET
		    email_enabled       = EXCLUDED.email_enabled,
		    webhook_enabled     = EXCLUDED.webhook_enabled,
		    webhook_url         = EXCLUDED.webhook_url,
		    webhook_secret      = CASE WHEN EXCLUDED.webhook_secret = '' THEN notification.settings.webhook_secret ELSE EXCLUDED.webhook_secret END,
		    reminders_enabled   = EXCLUDED.reminders_enabled,
		    reminder_lead_hours = EXCLUDED.reminder_lead_hours,
		    updated_at          = now()
	`, customerID, in.EmailEnabled, in.WebhookEnabled, strings.TrimSpace(in.WebhookURL), in.WebhookSecret,
		in.RemindersEnabled, in.ReminderLeadHours)
	if err != nil {
		return Settings{}, fmt.Errorf("update notification settings: %w", err)
	}
	out, _, err := s.GetSettings(ctx, customerID)
	return out, err
}

// ClearSettings removes a tenant's own row, so it inherits again.
func (s *Store) ClearSettings(ctx context.Context, customerID string) error {
	if customerID == DeploymentScope {
		return fmt.Errorf("the deployment's own settings cannot be cleared")
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM notification.settings WHERE customer_id = $1::uuid`, customerID)
	return err
}

// Defaults supplies the deployment's row from wherever it lives (the control
// plane, which in dedicated mode is another database than the tenant's), or
// reports that this edition has no deployment settings.
type Defaults func(ctx context.Context) (Settings, bool)

// DeploymentDefaults is the process's one resolver of the deployment row,
// set by cmd/gateway once the control plane and the licence are known. It
// is process-wide because the control plane is: every Store in the process
// — including the ones producers build ad hoc to call Notify — inherits
// from the same row. nil means no edition-level defaults.
var DeploymentDefaults Defaults

// maxReminderLead is the longest lead any scope in this database asks for,
// widened by the deployment's own — how far ahead the reminder has to look.
func (s *Store) maxReminderLead(ctx context.Context) (int32, error) {
	var lead int32
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(max(reminder_lead_hours), 0) FROM notification.settings WHERE reminders_enabled`).Scan(&lead); err != nil {
		return 0, fmt.Errorf("read reminder leads: %w", err)
	}
	if DeploymentDefaults != nil {
		if d, ok := DeploymentDefaults(ctx); ok && d.RemindersEnabled && d.ReminderLeadHours > lead {
			lead = d.ReminderLeadHours
		}
	}
	return lead, nil
}

// Effective is what applies to a tenant right now: its own row, else the
// deployment's when the edition includes one, else nothing on. inherited
// says which.
func (s *Store) Effective(ctx context.Context, customerID string, defaults Defaults) (settings Settings, inherited bool, err error) {
	settings, found, err := s.GetSettings(ctx, customerID)
	if err != nil || found || customerID == DeploymentScope {
		return settings, false, err
	}
	if defaults != nil {
		if d, ok := defaults(ctx); ok {
			return d, true, nil
		}
	}
	return Settings{}, false, nil
}

// CustomerOf resolves the tenant a notification belongs to: the tenant that
// owns the resource it is about, else the recipient's own, else — in a
// database that holds exactly one tenant — that one. Empty when none can be
// found, which the callers treat as "nothing outbound".
func (s *Store) CustomerOf(ctx context.Context, recipientUserID, resourceType, resourceID string) string {
	var cid *string
	switch resourceType {
	case "workflow_instance":
		_ = s.pool.QueryRow(ctx, `
			SELECT a.customer_id::text FROM workflow.workflow_instance i
			JOIN workflow.workflow_def d ON d.id = i.workflow_def_id
			JOIN core.application a ON a.id = d.application_id WHERE i.id = $1::uuid`, resourceID).Scan(&cid)
	case "automation_rule":
		_ = s.pool.QueryRow(ctx, `
			SELECT a.customer_id::text FROM workflow.automation_rule r
			JOIN core.application a ON a.id = r.application_id WHERE r.id = $1::uuid`, resourceID).Scan(&cid)
	case "application":
		_ = s.pool.QueryRow(ctx, `SELECT customer_id::text FROM core.application WHERE id = $1::uuid`, resourceID).Scan(&cid)
	}
	if cid != nil && *cid != "" {
		return *cid
	}
	cid = nil
	_ = s.pool.QueryRow(ctx, `SELECT customer_id::text FROM identity.user WHERE id = $1::uuid`, recipientUserID).Scan(&cid)
	if cid != nil && *cid != "" {
		return *cid
	}
	var n int
	var only *string
	if err := s.pool.QueryRow(ctx, `SELECT count(*), min(id::text) FROM core.customer`).Scan(&n, &only); err == nil && n == 1 && only != nil {
		return *only // exactly one tenant in this database
	}
	return ""
}
