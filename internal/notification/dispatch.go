package notification

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Outbound delivery.
//
// A notification on the email or webhook channel is written pending and left
// for the dispatcher, which claims due rows, delivers them and records the
// outcome. Delivery is at-least-once with a bounded number of attempts: a
// notification nobody could deliver becomes 'failed' with the reason on the
// row, never a silent gap.

// MaxAttempts is how often one notification is retried before it is failed.
const MaxAttempts = 5

// backoff is the delay before attempt n+1 (1 min, 5, 15, 60, then give up).
func backoff(attempts int) time.Duration {
	switch attempts {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

// Message is one notification ready to leave the platform.
type Message struct {
	ID           string
	Channel      string // "email" or "webhook"
	RecipientID  string
	Email        string
	DisplayName  string
	TemplateID   string
	Vars         map[string]string
	ResourceType string
	ResourceID   string
	CreatedAt    time.Time
}

// Subject is the notification's subject line, falling back to something
// readable when a producer did not set one.
func (m Message) Subject() string { return m.SubjectFor("") }

// SubjectFor is Subject with the tenant's own name in the fallback when the
// tenant is white-labelled.
func (m Message) SubjectFor(brand string) string {
	if s := strings.TrimSpace(m.Vars["subject"]); s != "" {
		return s
	}
	if brand != "" {
		return "Notification from " + brand
	}
	return "Notification from Mavericks Engine"
}

// Body is the notification's text.
func (m Message) Body() string {
	if b := strings.TrimSpace(m.Vars["message"]); b != "" {
		return b
	}
	return m.Subject()
}

// Mailer sends one e-mail. The interface exists so the dispatcher can be
// tested without an SMTP server.
type Mailer interface {
	Send(ctx context.Context, to, displayName, subject, body string) error
}

// SMTPConfig is the deployment's mail relay, read from the environment by
// cmd/gateway. Host empty means "no mail relay configured", and the
// dispatcher then fails e-mail notifications with that reason rather than
// pretending to deliver them.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	// StartTLS upgrades the connection before authenticating. On by default
	// for anything but an unauthenticated local relay.
	StartTLS bool
}

// SMTPMailer delivers through a standard SMTP relay.
type SMTPMailer struct{ Cfg SMTPConfig }

// NewMailer returns nil when no relay is configured, which the dispatcher
// treats as "e-mail cannot be delivered here".
func NewMailer(cfg SMTPConfig) Mailer {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.From == "" {
		cfg.From = "mavericks@localhost"
	}
	return &SMTPMailer{Cfg: cfg}
}

func (m *SMTPMailer) Send(ctx context.Context, to, displayName, subject, body string) error {
	return m.SendAs(ctx, "", to, displayName, subject, body)
}

// SendAs is Send with a display name on the From header, so a
// white-labelled tenant's mail arrives as "Acme Planning <relay address>".
func (m *SMTPMailer) SendAs(ctx context.Context, fromName, to, displayName, subject, body string) error {
	addr := fmt.Sprintf("%s:%d", m.Cfg.Host, m.Cfg.Port)
	recipient := to
	if displayName != "" {
		recipient = fmt.Sprintf("%s <%s>", displayName, to)
	}
	from := m.Cfg.From
	if fromName != "" {
		from = fmt.Sprintf("%q <%s>", strings.ReplaceAll(fromName, "\n", " "), m.Cfg.From)
	}
	msg := strings.Join([]string{
		"From: " + from,
		"To: " + recipient,
		"Subject: " + strings.ReplaceAll(subject, "\n", " "),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Date: " + time.Now().Format(time.RFC1123Z),
		"",
		body,
	}, "\r\n")

	var auth smtp.Auth
	if m.Cfg.Username != "" {
		auth = smtp.PlainAuth("", m.Cfg.Username, m.Cfg.Password, m.Cfg.Host)
	}
	// net/smtp has no context support; the deadline is the caller's timeout
	// around the whole dispatch pass.
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(addr, auth, m.Cfg.From, []string{to}, []byte(msg)) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// webhookPayload is the JSON a webhook receiver gets. Stable by contract:
// receivers parse it.
type webhookPayload struct {
	ID           string            `json:"id"`
	Template     string            `json:"template_id"`
	Subject      string            `json:"subject"`
	Message      string            `json:"message"`
	Recipient    string            `json:"recipient_user_id"`
	Email        string            `json:"recipient_email,omitempty"`
	ResourceType string            `json:"resource_type,omitempty"`
	ResourceID   string            `json:"resource_id,omitempty"`
	Vars         map[string]string `json:"vars"`
	CreatedAt    time.Time         `json:"created_at"`
}

// Dispatcher delivers the outbound notifications of one database.
type Dispatcher struct {
	Store  *Store
	Mailer Mailer
	// HTTP is used for webhooks; nil means a client with a 15s timeout.
	HTTP *http.Client
	Log  zerolog.Logger
	// Batch is how many notifications one pass claims (default 50).
	Batch int
	// BrandName, when set, names the recipient's tenant on outbound mail
	// (the subject fallback and the sender's display name) — white-labelling.
	// It receives the recipient's user id; the tenant is resolved from it.
	BrandName func(ctx context.Context, recipientUserID string) string
}

// BrandedMailer is a Mailer that can send under a display name.
type BrandedMailer interface {
	SendAs(ctx context.Context, fromName, to, displayName, subject, body string) error
}

func (d *Dispatcher) httpClient() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Run delivers every due notification, then repeats on interval until ctx
// ends. One of these runs per database.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := d.RunOnce(ctx); err != nil && ctx.Err() == nil {
			d.Log.Warn().Err(err).Msg("notification dispatch pass failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce delivers one batch and reports how many were delivered.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	batch := d.Batch
	if batch <= 0 {
		batch = 50
	}
	msgs, err := d.Store.claimOutbound(ctx, batch)
	if err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}
	settings, err := d.Store.GetSettings(ctx)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, m := range msgs {
		if dErr := d.deliver(ctx, settings, m); dErr != nil {
			if rErr := d.Store.recordFailure(ctx, m.ID, dErr.Error()); rErr != nil {
				d.Log.Warn().Err(rErr).Str("notification", m.ID).Msg("recording a delivery failure failed")
			}
			d.Log.Warn().Err(dErr).Str("notification", m.ID).Str("channel", m.Channel).Msg("notification delivery failed")
			continue
		}
		if rErr := d.Store.recordDelivered(ctx, m.ID); rErr != nil {
			d.Log.Warn().Err(rErr).Str("notification", m.ID).Msg("recording a delivery failed")
			continue
		}
		delivered++
	}
	return delivered, nil
}

func (d *Dispatcher) deliver(ctx context.Context, settings Settings, m Message) error {
	switch m.Channel {
	case "email":
		if !settings.EmailEnabled {
			return fmt.Errorf("e-mail notifications are turned off")
		}
		if d.Mailer == nil {
			return fmt.Errorf("no mail relay is configured for this deployment (SMTP_HOST)")
		}
		if m.Email == "" {
			return fmt.Errorf("recipient has no e-mail address")
		}
		brand := ""
		if d.BrandName != nil {
			brand = d.BrandName(ctx, m.RecipientID)
		}
		if bm, ok := d.Mailer.(BrandedMailer); ok && brand != "" {
			return bm.SendAs(ctx, brand, m.Email, m.DisplayName, m.SubjectFor(brand), m.Body())
		}
		return d.Mailer.Send(ctx, m.Email, m.DisplayName, m.SubjectFor(brand), m.Body())
	case "webhook":
		if !settings.WebhookEnabled || settings.WebhookURL == "" {
			return fmt.Errorf("webhook notifications are turned off")
		}
		return d.postWebhook(ctx, settings, m)
	default:
		return fmt.Errorf("channel %q has no sender", m.Channel)
	}
}

func (d *Dispatcher) postWebhook(ctx context.Context, settings Settings, m Message) error {
	body, err := json.Marshal(webhookPayload{
		ID: m.ID, Template: m.TemplateID, Subject: m.Subject(), Message: m.Body(),
		Recipient: m.RecipientID, Email: m.Email,
		ResourceType: m.ResourceType, ResourceID: m.ResourceID,
		Vars: m.Vars, CreatedAt: m.CreatedAt,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, settings.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mavericks-engine")
	// Let a receiver prove the delivery came from this deployment.
	if secret, sErr := d.Store.webhookSecret(ctx); sErr == nil && secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Mavericks-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	// Read a little of the body so a failure names what the receiver said.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// ── store helpers for the dispatcher ────────────────────────────────────────

// claimOutbound takes the next due pending notifications off the outbound
// channels and pushes their next attempt out, so a second dispatcher (another
// gateway replica) does not pick up the same rows. Claiming and delivering
// are separate: a crash after claiming costs a delay, never a duplicate
// storm.
func (s *Store) claimOutbound(ctx context.Context, limit int) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
		    SELECT id FROM notification.notification
		    WHERE status = 'pending' AND channel <> 'in_app' AND next_attempt_at <= now()
		    ORDER BY next_attempt_at
		    LIMIT $1
		    FOR UPDATE SKIP LOCKED
		)
		UPDATE notification.notification n
		SET attempts = n.attempts + 1, next_attempt_at = now() + interval '5 minutes'
		FROM due
		WHERE n.id = due.id
		RETURNING n.id::text, n.channel::text, n.recipient_user_id::text, n.template_id,
		          n.template_vars, COALESCE(n.resource_type,''), COALESCE(n.resource_id,''), n.created_at,
		          COALESCE((SELECT email FROM identity.user u WHERE u.id = n.recipient_user_id), ''),
		          COALESCE((SELECT display_name FROM identity.user u WHERE u.id = n.recipient_user_id), '')
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbound notifications: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var varsJSON []byte
		if err := rows.Scan(&m.ID, &m.Channel, &m.RecipientID, &m.TemplateID, &varsJSON,
			&m.ResourceType, &m.ResourceID, &m.CreatedAt, &m.Email, &m.DisplayName); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(varsJSON, &m.Vars)
		if m.Vars == nil {
			m.Vars = map[string]string{}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) recordDelivered(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE notification.notification
		SET status = 'delivered', delivered_at = now(), last_error = ''
		WHERE id = $1::uuid`, id)
	return err
}

// recordFailure schedules the next attempt, or gives up once MaxAttempts is
// reached, keeping the reason on the row either way.
func (s *Store) recordFailure(ctx context.Context, id, reason string) error {
	var attempts int
	if err := s.pool.QueryRow(ctx,
		`SELECT attempts FROM notification.notification WHERE id = $1::uuid`, id).Scan(&attempts); err != nil {
		return err
	}
	if attempts >= MaxAttempts {
		_, err := s.pool.Exec(ctx, `
			UPDATE notification.notification SET status = 'failed', last_error = $2
			WHERE id = $1::uuid`, id, reason)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE notification.notification SET last_error = $2, next_attempt_at = now() + $3::interval
		WHERE id = $1::uuid`, id, reason, backoff(attempts).String())
	return err
}

func (s *Store) webhookSecret(ctx context.Context) (string, error) {
	var secret string
	err := s.pool.QueryRow(ctx, `SELECT webhook_secret FROM notification.settings WHERE id = TRUE`).Scan(&secret)
	return secret, err
}
