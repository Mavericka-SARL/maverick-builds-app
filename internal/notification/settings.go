package notification

import (
	"context"
	"fmt"
	"strings"
)

// Settings is the one row of notification.settings: which outbound channels
// this database delivers on, and whether overdue tasks are reminded.
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
}

// GetSettings reads the row, which the migration guarantees exists.
func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	var out Settings
	var secret string
	err := s.pool.QueryRow(ctx, `
		SELECT email_enabled, webhook_enabled, webhook_url, webhook_secret,
		       reminders_enabled, reminder_lead_hours
		FROM notification.settings WHERE id = TRUE
	`).Scan(&out.EmailEnabled, &out.WebhookEnabled, &out.WebhookURL, &secret,
		&out.RemindersEnabled, &out.ReminderLeadHours)
	if err != nil {
		return Settings{}, fmt.Errorf("read notification settings: %w", err)
	}
	out.HasWebhookSecret = secret != ""
	return out, nil
}

// UpdateSettings writes the row. An empty WebhookSecret keeps the stored one,
// so a console that never receives the secret can still save the form.
func (s *Store) UpdateSettings(ctx context.Context, in Settings) (Settings, error) {
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
		UPDATE notification.settings SET
		    email_enabled       = $1,
		    webhook_enabled     = $2,
		    webhook_url         = $3,
		    webhook_secret      = CASE WHEN $4 = '' THEN webhook_secret ELSE $4 END,
		    reminders_enabled   = $5,
		    reminder_lead_hours = $6,
		    updated_at          = now()
		WHERE id = TRUE
	`, in.EmailEnabled, in.WebhookEnabled, strings.TrimSpace(in.WebhookURL), in.WebhookSecret,
		in.RemindersEnabled, in.ReminderLeadHours)
	if err != nil {
		return Settings{}, fmt.Errorf("update notification settings: %w", err)
	}
	return s.GetSettings(ctx)
}
