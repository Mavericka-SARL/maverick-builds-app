package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type sentMail struct{ To, Name, Subject, Body string }

type fakeMailer struct {
	mu   sync.Mutex
	sent []sentMail
	fail error
}

func (f *fakeMailer) Send(_ context.Context, to, name, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, sentMail{to, name, subject, body})
	return nil
}

func (f *fakeMailer) all() []sentMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMail(nil), f.sent...)
}

func setupStore(t *testing.T) (*Store, *pgxpool.Pool, string) {
	t.Helper()
	pool := testdb.New(t, migrationfs.FS, ".")
	var userID string
	// The user belongs to a tenant: outbound channels are the tenant's
	// settings (migration 091), and a user of no tenant gets in-app only.
	if err := pool.QueryRow(context.Background(),
		`WITH c AS (INSERT INTO core.customer (name, plan) VALUES ('Notify Co', 'standard') RETURNING id)
		 INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		 SELECT 'notif-user', 'jo@example.test', 'Jo Planner', c.id FROM c RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	return NewStore(pool), pool, userID
}

func statusOf(t *testing.T, pool *pgxpool.Pool, id string) (status, lastErr string, attempts int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT status::text, last_error, attempts FROM notification.notification WHERE id=$1::uuid`, id,
	).Scan(&status, &lastErr, &attempts); err != nil {
		t.Fatal(err)
	}
	return
}

// Turning a channel on is what makes a notification leave the platform; with
// everything off, a notification is still written for the console and nothing
// is sent anywhere.
func TestNotifyFansOutOnlyToEnabledChannels(t *testing.T) {
	ctx := context.Background()
	store, pool, userID := setupStore(t)

	if _, err := store.Notify(ctx, userID, "workflow_step_notification",
		map[string]string{"subject": "Budget submitted", "message": "Please review."}, "workflow_instance", "wf-1"); err != nil {
		t.Fatal(err)
	}
	var channels []string
	rows, err := pool.Query(ctx, `SELECT channel::text FROM notification.notification ORDER BY channel`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		channels = append(channels, c)
	}
	rows.Close()
	if len(channels) != 1 || channels[0] != "in_app" {
		t.Fatalf("with every outbound channel off, channels = %v", channels)
	}

	if _, err := store.UpdateSettings(ctx, tenantOf(t, store, userID), Settings{
		EmailEnabled: true, WebhookEnabled: true, WebhookURL: "https://hooks.example.test/mvx",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Notify(ctx, userID, "workflow_step_notification",
		map[string]string{"subject": "Second", "message": "Also review."}, "workflow_instance", "wf-2"); err != nil {
		t.Fatal(err)
	}
	var email, webhook, inApp int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE channel='email'),
		       count(*) FILTER (WHERE channel='webhook'),
		       count(*) FILTER (WHERE channel='in_app')
		FROM notification.notification`).Scan(&email, &webhook, &inApp); err != nil {
		t.Fatal(err)
	}
	if email != 1 || webhook != 1 || inApp != 2 {
		t.Fatalf("fan-out wrote email=%d webhook=%d in_app=%d", email, webhook, inApp)
	}
	// in-app is delivered by being written; the outbound rows wait.
	var pending int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification.notification WHERE status='pending'`).Scan(&pending)
	if pending != 2 {
		t.Fatalf("pending outbound rows = %d, want 2", pending)
	}
}

func TestDispatcherDeliversEmailAndWebhook(t *testing.T) {
	ctx := context.Background()
	store, pool, userID := setupStore(t)

	var got struct {
		mu        sync.Mutex
		body      []byte
		signature string
		hits      int
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.mu.Lock()
		defer got.mu.Unlock()
		got.hits++
		got.signature = r.Header.Get("X-Mavericks-Signature")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		got.body = buf
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if _, err := store.UpdateSettings(ctx, tenantOf(t, store, userID), Settings{
		EmailEnabled: true, WebhookEnabled: true, WebhookURL: srv.URL, WebhookSecret: "s3cret",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Notify(ctx, userID, "workflow_step_notification",
		map[string]string{"subject": "Approval needed", "message": "The FY2027 budget is waiting."}, "workflow_instance", "wf-7"); err != nil {
		t.Fatal(err)
	}

	mailer := &fakeMailer{}
	d := &Dispatcher{Store: store, Mailer: mailer, Log: logger.New("test")}
	delivered, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if delivered != 2 {
		t.Fatalf("delivered %d, want 2", delivered)
	}

	sent := mailer.all()
	if len(sent) != 1 || sent[0].To != "jo@example.test" || sent[0].Name != "Jo Planner" ||
		sent[0].Subject != "Approval needed" || !strings.Contains(sent[0].Body, "FY2027") {
		t.Fatalf("e-mail was not what the notification said: %+v", sent)
	}

	got.mu.Lock()
	defer got.mu.Unlock()
	if got.hits != 1 {
		t.Fatalf("webhook hits = %d", got.hits)
	}
	var payload webhookPayload
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("webhook body is not the documented JSON: %v (%s)", err, got.body)
	}
	if payload.Subject != "Approval needed" || payload.ResourceID != "wf-7" || payload.Email != "jo@example.test" {
		t.Fatalf("webhook payload = %+v", payload)
	}
	if !strings.HasPrefix(got.signature, "sha256=") {
		t.Fatalf("webhook was not signed: %q", got.signature)
	}

	var stillPending int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification.notification WHERE status='pending'`).Scan(&stillPending)
	if stillPending != 0 {
		t.Fatalf("%d notifications left pending after delivery", stillPending)
	}
	// A second pass has nothing to do: delivery is not repeated.
	if n, err := d.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second pass delivered %d (%v)", n, err)
	}
}

// A notification nobody could deliver must end up failed with the reason on
// it, after a bounded number of attempts — never silently dropped, never
// retried forever.
func TestDeliveryRetriesThenFails(t *testing.T) {
	ctx := context.Background()
	store, pool, userID := setupStore(t)
	if _, err := store.UpdateSettings(ctx, tenantOf(t, store, userID), Settings{EmailEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Notify(ctx, userID, "t", map[string]string{"subject": "s"}, "", ""); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM notification.notification WHERE channel='email'`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	d := &Dispatcher{Store: store, Mailer: &fakeMailer{fail: errRelayDown}, Log: logger.New("test")}
	for i := 1; i <= MaxAttempts; i++ {
		if _, err := d.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		status, lastErr, attempts := statusOf(t, pool, id)
		if !strings.Contains(lastErr, "relay is down") {
			t.Fatalf("attempt %d lost the reason: %q", i, lastErr)
		}
		if i < MaxAttempts {
			if status != "pending" {
				t.Fatalf("attempt %d/%d already %s", i, MaxAttempts, status)
			}
			if attempts != i {
				t.Fatalf("attempts = %d after pass %d", attempts, i)
			}
			// The backoff pushed the next attempt into the future; bring it
			// back so the next pass picks the row up again.
			if _, err := pool.Exec(ctx,
				`UPDATE notification.notification SET next_attempt_at = now() WHERE id=$1::uuid`, id); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if status != "failed" {
			t.Fatalf("after %d attempts status = %s, want failed", MaxAttempts, status)
		}
	}
	// A failed notification is not claimed again.
	if _, err := pool.Exec(ctx, `UPDATE notification.notification SET next_attempt_at = now() WHERE id=$1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := d.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("a failed notification was retried (%d, %v)", n, err)
	}
}

// Turning a channel off after rows were queued must not deliver them.
func TestDisabledChannelIsNotDelivered(t *testing.T) {
	ctx := context.Background()
	store, pool, userID := setupStore(t)
	if _, err := store.UpdateSettings(ctx, tenantOf(t, store, userID), Settings{EmailEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Notify(ctx, userID, "t", map[string]string{"subject": "s"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateSettings(ctx, tenantOf(t, store, userID), Settings{EmailEnabled: false}); err != nil {
		t.Fatal(err)
	}
	mailer := &fakeMailer{}
	d := &Dispatcher{Store: store, Mailer: mailer, Log: logger.New("test")}
	if n, _ := d.RunOnce(ctx); n != 0 || len(mailer.all()) != 0 {
		t.Fatalf("delivered %d notifications on a channel that is off", n)
	}
	var id, lastErr string
	_ = pool.QueryRow(ctx, `SELECT id::text, last_error FROM notification.notification WHERE channel='email'`).Scan(&id, &lastErr)
	if !strings.Contains(lastErr, "turned off") {
		t.Fatalf("reason = %q", lastErr)
	}
}

func TestSettingsValidationAndSecretHandling(t *testing.T) {
	ctx := context.Background()
	store, _, userID := setupStore(t)

	if _, err := store.UpdateSettings(ctx, tenantOf(t, store, userID), Settings{WebhookEnabled: true, WebhookURL: "hooks.example.test"}); err == nil {
		t.Fatal("a webhook url without a scheme was accepted")
	}
	if _, err := store.UpdateSettings(ctx, tenantOf(t, store, userID), Settings{ReminderLeadHours: -1}); err == nil {
		t.Fatal("a negative lead time was accepted")
	}
	got, err := store.UpdateSettings(ctx, tenantOf(t, store, userID), Settings{
		WebhookEnabled: true, WebhookURL: "https://hooks.example.test/x", WebhookSecret: "first",
		RemindersEnabled: true, ReminderLeadHours: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasWebhookSecret || got.WebhookSecret != "" {
		t.Fatalf("the secret must be reported as set but never returned: %+v", got)
	}
	// Saving without the secret keeps the stored one, so a console that never
	// receives it can still save the form.
	if _, err := store.UpdateSettings(ctx, tenantOf(t, store, userID), Settings{
		WebhookEnabled: true, WebhookURL: "https://hooks.example.test/x", RemindersEnabled: true, ReminderLeadHours: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _, err := store.GetSettings(ctx, tenantOf(t, store, userID)); err != nil || got.webhookSecretValue != "first" {
		t.Fatalf("secret after a save without one = %q (%v)", got.webhookSecretValue, err)
	}
}

var errRelayDown = &relayError{}

type relayError struct{}

func (*relayError) Error() string { return "the relay is down" }

func TestBackoffGrows(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		d := backoff(attempt)
		if d < prev {
			t.Fatalf("backoff shrank at attempt %d: %s after %s", attempt, d, prev)
		}
		prev = d
	}
}

// tenantOf is the tenant the fixture user belongs to — the scope whose
// settings decide their outbound channels.
func tenantOf(t *testing.T, store *Store, userID string) string {
	t.Helper()
	cid := store.CustomerOf(context.Background(), userID, "", "")
	if cid == "" {
		t.Fatal("fixture user has no tenant")
	}
	return cid
}
