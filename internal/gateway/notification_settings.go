package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/ee/branding"
	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/license"
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
	scope, ok := h.settingsScopeFor(w, r, act, true)
	if !ok {
		return
	}
	store := notification.NewStore(h.db.For(ctx))

	switch r.Method {
	case http.MethodGet:
		settings, inherited, err := store.Effective(ctx, scope.CustomerID, notification.DeploymentDefaults)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		scope.Inherited = inherited
		jsonOK(w, withMailerState(settings, h.mailer != nil, scope))
	case http.MethodPut:
		var body notification.Settings
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		settings, err := store.UpdateSettings(ctx, scope.CustomerID, body)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventNotificationSettingsUpdated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "notification_settings", ResourceID: scopeResourceID(scope),
			Metadata: map[string]string{
				"scope":     scopeWord(scope),
				"email":     boolWord(settings.EmailEnabled),
				"webhook":   boolWord(settings.WebhookEnabled),
				"reminders": boolWord(settings.RemindersEnabled),
			},
		})
		jsonOK(w, withMailerState(settings, h.mailer != nil, scope))
	case http.MethodDelete:
		// A tenant drops its own row and inherits the deployment's again.
		if err := store.ClearSettings(ctx, scope.CustomerID); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventNotificationSettingsUpdated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "notification_settings", ResourceID: scopeResourceID(scope),
			Metadata: map[string]string{"scope": scopeWord(scope), "cleared": "true"},
		})
		settings, inherited, err := store.Effective(ctx, scope.CustomerID, notification.DeploymentDefaults)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		scope.Inherited = inherited
		jsonOK(w, withMailerState(settings, h.mailer != nil, scope))
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// settingsResponse adds what the console cannot know from the row alone:
// whether the deployment has a mail relay at all, and whose settings these
// are. Turning e-mail on without a relay queues notifications that can
// never be delivered, so the console says so up front.
type settingsResponse struct {
	notification.Settings
	MailerConfigured bool          `json:"mailer_configured"`
	Scope            settingsScope `json:"scope"`
}

func withMailerState(s notification.Settings, configured bool, scope settingsScope) settingsResponse {
	s.WebhookSecret = "" // never returned
	return settingsResponse{Settings: s, MailerConfigured: configured, Scope: scope}
}

// scopeResourceID names the row an audit event is about.
func scopeResourceID(scope settingsScope) string {
	if scope.Deployment {
		return "deployment"
	}
	return scope.CustomerID
}

func scopeWord(scope settingsScope) string {
	if scope.Deployment {
		return "deployment"
	}
	return "tenant"
}

func boolWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// notificationTestSend mails the calling administrator through the
// deployment's relay, synchronously, and returns the relay's verdict. It is
// the only way to know the relay works: reading the configuration back
// proves it was stored, and the dispatcher's failures surface one at a time,
// on real notifications, hours later. The recipient is fixed to the caller's
// own address — an administrator cannot use the relay to mail anyone else.
func (h *handler) notificationTestSend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	if h.mailer == nil {
		jsonErr(w, fmt.Errorf("no mail relay is configured for this deployment (SMTP_HOST)"), http.StatusConflict)
		return
	}
	if strings.TrimSpace(act.Email) == "" {
		jsonErr(w, fmt.Errorf("your account has no e-mail address to send to"), http.StatusConflict)
		return
	}
	// A relay that accepts the connection and then stalls is the common
	// failure (a NetworkPolicy dropping the port looks exactly like that);
	// bound it so the console gets an answer instead of a hung request.
	sendCtx, cancel := context.WithTimeout(ctx, notificationTestSendTimeout)
	defer cancel()

	// The same sender rule as the dispatcher: the tenant's own name when it
	// is white-labelled, the platform's otherwise.
	brand := ""
	if h.lic != nil && h.lic.Has(license.FeatureWhiteLabel) {
		brand = branding.EmailNameForUser(ctx, h.db.For(ctx), act.UserID)
	}
	sender := brand
	if sender == "" {
		sender = notification.PlatformName
	}
	subject := "Test message from " + sender
	body := "This message confirms that " + sender + " can deliver e-mail notifications to " + act.Email + ".\r\n\r\n" +
		"It was requested by " + act.Name + " from the notification delivery settings; nothing else was sent."
	var err error
	if bm, isBranded := h.mailer.(notification.BrandedMailer); isBranded && brand != "" {
		err = bm.SendAs(sendCtx, brand, act.Email, act.Name, subject, body)
	} else {
		err = h.mailer.Send(sendCtx, act.Email, act.Name, subject, body)
	}
	outcome := "sent"
	if err != nil {
		outcome = "failed"
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventNotificationTestSent,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "notification_settings", ResourceID: "settings",
		Metadata: map[string]string{"to": act.Email, "outcome": outcome},
	})
	if err != nil {
		// The relay's own words reach the console: "535 authentication
		// failed", "450 domain not verified" and a timeout each want a
		// different fix.
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("no answer within %s — check that the gateway may reach the relay (NetworkPolicy egress on the SMTP port) and that the host and port are right", notificationTestSendTimeout)
		}
		jsonErr(w, fmt.Errorf("the relay did not accept the message: %w", err), http.StatusBadGateway)
		return
	}
	jsonOK(w, map[string]string{"sent_to": act.Email})
}

// notificationTestSendTimeout bounds one synchronous test send: longer than
// a healthy relay ever needs, shorter than the console's patience.
const notificationTestSendTimeout = 20 * time.Second
