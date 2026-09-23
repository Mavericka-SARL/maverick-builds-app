package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// recordingMailer stands in for the relay: it succeeds, refuses, or stalls
// until the caller gives up — the three things a real one does.
type recordingMailer struct {
	mu    sync.Mutex
	sent  []string // "to|name|subject"
	fail  error
	stall bool
}

func (m *recordingMailer) Send(ctx context.Context, to, name, subject, _ string) error {
	if m.stall {
		<-ctx.Done()
		return ctx.Err()
	}
	if m.fail != nil {
		return m.fail
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, to+"|"+name+"|"+subject)
	return nil
}

// POST /api/notifications/settings/test mails the caller — only the caller —
// through the relay and returns its verdict; the settings screen reports
// whether a relay exists at all.
func TestNotificationTestSend(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	mk := func(sub, email, role string) {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, $2, $1) RETURNING id::text`, sub, email,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, $2)`, id, role); err != nil {
			t.Fatal(err)
		}
	}
	var custID string
	if err := pool.QueryRow(ctx, `INSERT INTO core.customer (name, plan) VALUES ('Notify Co', 'enterprise') RETURNING id::text`).Scan(&custID); err != nil {
		t.Fatal(err)
	}
	mk("nt-admin", "admin@notify.dev", "platform_admin")
	mk("nt-tenant", "ta@notify.dev", "tenant_admin")
	mk("nt-dev", "dev@notify.dev", "developer")
	if _, err := pool.Exec(ctx, `UPDATE identity.user SET customer_id = $1::uuid WHERE keycloak_sub IN ('nt-tenant', 'nt-dev')`, custID); err != nil {
		t.Fatal(err)
	}

	call := func(t *testing.T, srv *httptest.Server, method, path, persona string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, nil)
		req.Header.Set("X-Dev-User", persona)
		if persona == "nt-admin" {
			// The platform admin acts on the tenant here, as the console does
			// from the tenant's card; the deployment row has its own test.
			req.Header.Set(tenantHeader, custID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	serve := func(m notification.Mailer) *httptest.Server {
		var mailer notification.Mailer
		if m != nil {
			mailer = m
		}
		return httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Mailer: mailer}))
	}

	t.Run("no relay: settings say so and a test send is refused", func(t *testing.T) {
		srv := serve(nil)
		defer srv.Close()
		code, body := call(t, srv, http.MethodGet, "/api/notifications/settings", "nt-admin")
		if code != http.StatusOK || body["mailer_configured"] != false {
			t.Fatalf("settings: code=%d body=%v", code, body)
		}
		code, body = call(t, srv, http.MethodPost, "/api/notifications/settings/test", "nt-admin")
		if code != http.StatusConflict || !strings.Contains(body["error"].(string), "SMTP_HOST") {
			t.Fatalf("test send without relay: code=%d body=%v", code, body)
		}
	})

	t.Run("relay accepts: the caller, and only the caller, is mailed", func(t *testing.T) {
		m := &recordingMailer{}
		srv := serve(m)
		defer srv.Close()
		code, body := call(t, srv, http.MethodGet, "/api/notifications/settings", "nt-admin")
		if code != http.StatusOK || body["mailer_configured"] != true {
			t.Fatalf("settings: code=%d body=%v", code, body)
		}
		for _, persona := range []string{"nt-admin", "nt-tenant"} {
			code, body = call(t, srv, http.MethodPost, "/api/notifications/settings/test", persona)
			if code != http.StatusOK {
				t.Fatalf("%s: code=%d body=%v", persona, code, body)
			}
		}
		if len(m.sent) != 2 || !strings.HasPrefix(m.sent[0], "admin@notify.dev|nt-admin|Test message from "+notification.PlatformName) ||
			!strings.HasPrefix(m.sent[1], "ta@notify.dev|nt-tenant|") {
			t.Fatalf("sent=%v", m.sent)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type = 'notification.test_sent' AND metadata->>'outcome' = 'sent'`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("audit rows=%d err=%v", n, err)
		}
	})

	t.Run("relay refuses: its own words come back as 502", func(t *testing.T) {
		m := &recordingMailer{fail: errors.New("535 5.7.8 authentication failed")}
		srv := serve(m)
		defer srv.Close()
		code, body := call(t, srv, http.MethodPost, "/api/notifications/settings/test", "nt-admin")
		if code != http.StatusBadGateway || !strings.Contains(body["error"].(string), "535 5.7.8 authentication failed") {
			t.Fatalf("code=%d body=%v", code, body)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type = 'notification.test_sent' AND metadata->>'outcome' = 'failed'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("audit rows=%d err=%v", n, err)
		}
	})

	t.Run("relay stalls: bounded, and the answer names the likely cause", func(t *testing.T) {
		m := &recordingMailer{stall: true}
		h := NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Mailer: m})
		// Shorten the deadline through the request context rather than
		// waiting the real 20 s; the handler's own timeout still applies.
		reqCtx, cancel := context.WithTimeout(ctx, 200_000_000) // 200 ms
		defer cancel()
		req := httptest.NewRequestWithContext(reqCtx, http.MethodPost, "/api/notifications/settings/test", nil)
		req.Header.Set("X-Dev-User", "nt-admin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "NetworkPolicy") {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("not an administrator: forbidden, nothing sent", func(t *testing.T) {
		m := &recordingMailer{}
		srv := serve(m)
		defer srv.Close()
		code, _ := call(t, srv, http.MethodPost, "/api/notifications/settings/test", "nt-dev")
		if code != http.StatusForbidden || len(m.sent) != 0 {
			t.Fatalf("code=%d sent=%v", code, m.sent)
		}
	})
}
