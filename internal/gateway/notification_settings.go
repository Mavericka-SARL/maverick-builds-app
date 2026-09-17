package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Outbound notification settings: which channels this tenant delivers on and
// whether overdue tasks are reminded. Administrators only — a webhook URL is
// where this deployment's data goes, and the reminder switch pages real
// people.
//
// The SMTP relay is deliberately NOT configurable here; it belongs to the
// deployment's environment (see docs/NOTIFICATIONS.md), so a tenant admin
// cannot redirect the platform's mail.
func (h *handler) notificationSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	store := notification.NewStore(h.db.For(ctx))

	switch r.Method {
	case http.MethodGet:
		settings, err := store.GetSettings(ctx)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, withMailerState(settings, h.mailerConfigured))
	case http.MethodPut:
		var body notification.Settings
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		settings, err := store.UpdateSettings(ctx, body)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventNotificationSettingsUpdated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "notification_settings", ResourceID: "settings",
			Metadata: map[string]string{
				"email":     boolWord(settings.EmailEnabled),
				"webhook":   boolWord(settings.WebhookEnabled),
				"reminders": boolWord(settings.RemindersEnabled),
			},
		})
		jsonOK(w, withMailerState(settings, h.mailerConfigured))
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// settingsResponse adds what the console cannot know from the row alone:
// whether the deployment has a mail relay at all. Turning e-mail on without
// one queues notifications that can never be delivered, so the console says
// so up front.
type settingsResponse struct {
	notification.Settings
	MailerConfigured bool `json:"mailer_configured"`
}

func withMailerState(s notification.Settings, configured bool) settingsResponse {
	s.WebhookSecret = "" // never returned
	return settingsResponse{Settings: s, MailerConfigured: configured}
}

func boolWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
