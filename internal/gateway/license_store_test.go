package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/secretbox"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A platform administrator installs a license key under Platform › License
// (owner, 2026-11-22): the gateway verifies it, keeps it encrypted on the
// control plane and puts it in force at once; another replica (or the next
// start) picks it up; removing it returns the deployment to the Community
// Edition. Only a platform administrator may, and a key in the server's
// configuration wins over the console.
func TestLicenseKeyInstalledInTheConsole(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	t.Setenv(secretbox.EnvVar, "license-store-test-secret")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('lk-padmin', 'p@lk.test', 'Platform')`)
	exec(`INSERT INTO identity.role_assignment (user_id, role) SELECT id, 'platform_admin' FROM identity.user WHERE keycloak_sub = 'lk-padmin'`)
	exec(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('lk-tadmin', 't@lk.test', 'Tenant')`)
	exec(`INSERT INTO identity.role_assignment (user_id, role) SELECT id, 'tenant_admin' FROM identity.user WHERE keycloak_sub = 'lk-tadmin'`)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := license.EncodeKey(pub)
	sign := func(id string, ed license.Edition, expires time.Time) string {
		t.Helper()
		tok, err := license.Sign(license.Claims{ID: id, Edition: ed, Customer: "Acme Corp", Order: "ORD-2026-900",
			Agreement: "PFA 2026-10", IssuedAt: time.Now().Add(-time.Hour), ExpiresAt: expires}, priv)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	inAYear := time.Now().Add(365 * 24 * time.Hour)
	commercial := sign("lic-c", license.EditionCommercial, inAYear)
	enterprise := sign("lic-e", license.EditionEnterprise, inAYear)

	lic := license.Load(license.Options{PublicKey: pubB64})
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Plans: plan.NewEnforcer(pool), License: lic}))
	t.Cleanup(srv.Close)
	install := func(persona, key string) (int, map[string]any) {
		t.Helper()
		return callJSON(t, srv, persona, http.MethodPut, "/api/admin/license", map[string]any{"key": key})
	}
	edition := func() string {
		t.Helper()
		_, st := callJSON(t, srv, "lk-tadmin", http.MethodGet, "/api/license", nil)
		return st["edition"].(string) + "/" + st["source"].(string)
	}
	// Another replica: its own manager, the same database.
	replica := license.Load(license.Options{PublicKey: pubB64})
	sync := func() (bool, string) {
		t.Helper()
		changed, err := ApplyStoredLicense(ctx, pool, replica)
		if err != nil {
			t.Fatal(err)
		}
		st := replica.Status()
		return changed, string(st.Edition) + "/" + st.Source
	}

	// Refusals.
	if code, _ := install("lk-tadmin", commercial); code != http.StatusForbidden {
		t.Fatalf("a tenant admin installing a key: %d", code)
	}
	if code, body := install("lk-padmin", "MVX1.not.a-key"); code != http.StatusBadRequest {
		t.Fatalf("a malformed key: %d %v", code, body)
	}
	if code, body := install("lk-padmin", sign("lic-old", license.EditionEnterprise, time.Now().Add(-time.Hour))); code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "expired on") {
		t.Fatalf("an expired key: %d %v", code, body)
	}
	if got := edition(); got != "community/none" {
		t.Fatalf("after refusals the deployment runs %s", got)
	}

	// Install Commercial: in force at once, stored encrypted, offered to tenants.
	if code, body := install("lk-padmin", "  "+commercial+"\n"); code != 200 || body["edition"] != "commercial" || body["source"] != "console" {
		t.Fatalf("install commercial: %d %v", code, body)
	}
	if got := edition(); got != "commercial/console" {
		t.Fatalf("after install the deployment runs %s", got)
	}
	var stored, licID string
	if err := pool.QueryRow(ctx, `SELECT token, license_id FROM platform.license_key`).Scan(&stored, &licID); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "enc:v1:") || strings.Contains(stored, "MVX1") || licID != "lic-c" {
		t.Fatalf("stored row: %q… id %s", stored[:12], licID)
	}
	if _, plans := callJSONList(t, srv, "lk-padmin", "/api/admin/plans"); len(plans) != 2 || plans[1]["key"] != "commercial" || plans[1]["default"] != true {
		t.Fatalf("editions offered with the installed Commercial key: %v", plans)
	}
	if changed, got := sync(); !changed || got != "commercial/console" {
		t.Fatalf("replica after install: %v %s", changed, got)
	}
	if changed, _ := sync(); changed {
		t.Fatal("replica re-applied an unchanged key")
	}

	// Replace with Enterprise; a tenant created now starts on it.
	if code, body := install("lk-padmin", enterprise); code != 200 || body["edition"] != "enterprise" {
		t.Fatalf("install enterprise: %d %v", code, body)
	}
	if changed, got := sync(); !changed || got != "enterprise/console" {
		t.Fatalf("replica after the second install: %v %s", changed, got)
	}
	code, created := callJSON(t, srv, "lk-padmin", http.MethodPost, "/api/admin/tenants", map[string]any{"name": "Big Co", "getting_started": false})
	bigCo, _ := created["id"].(string)
	if code != 200 || bigCo == "" {
		t.Fatalf("create tenant: %d %v", code, created)
	}
	editionOf := func(id string) string {
		t.Helper()
		var p string
		if err := pool.QueryRow(ctx, `SELECT plan FROM core.customer WHERE id = $1::uuid`, id).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := editionOf(bigCo); got != "enterprise" {
		t.Fatalf("a tenant created under the Enterprise key is on %s", got)
	}
	// A lower key moves tenants above it down, at once (Change edition is
	// not there to do it once the key is gone); a higher one moves nobody up.
	if code, body := install("lk-padmin", commercial); code != 200 || body["edition"] != "commercial" {
		t.Fatalf("install commercial over enterprise: %d %v", code, body)
	}
	if got := editionOf(bigCo); got != "commercial" {
		t.Fatalf("after the Commercial key Big Co is on %s", got)
	}
	if code, body := install("lk-padmin", enterprise); code != 200 || body["edition"] != "enterprise" {
		t.Fatalf("install enterprise again: %d %v", code, body)
	}
	if got := editionOf(bigCo); got != "commercial" {
		t.Fatalf("a higher key moved Big Co up to %s", got)
	}

	// Remove.
	if code, _ := callJSON(t, srv, "lk-tadmin", http.MethodDelete, "/api/admin/license", nil); code != http.StatusForbidden {
		t.Fatalf("a tenant admin removing the key: %d", code)
	}
	if code, body := callJSON(t, srv, "lk-padmin", http.MethodDelete, "/api/admin/license", nil); code != 200 || body["edition"] != "community" {
		t.Fatalf("remove: %d %v", code, body)
	}
	if got := edition(); got != "community/none" {
		t.Fatalf("after removal the deployment runs %s", got)
	}
	if got := editionOf(bigCo); got != "community" {
		t.Fatalf("after removal Big Co is on %s", got)
	}
	if changed, got := sync(); !changed || got != "community/none" {
		t.Fatalf("replica after removal: %v %s", changed, got)
	}

	// Every change is audited.
	var installed, removed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE event_type = 'license.installed'), count(*) FILTER (WHERE event_type = 'license.removed')
		FROM audit.audit_event WHERE resource_type = 'license'`).Scan(&installed, &removed); err != nil {
		t.Fatal(err)
	}
	if installed != 4 || removed != 1 {
		t.Fatalf("audit: %d installed, %d removed; want 4 and 1", installed, removed)
	}

	// A key in the server's configuration wins: the console can neither
	// replace nor remove it, and a stored key is not read over it.
	exec(`INSERT INTO platform.license_key (token, license_id, edition, expires_at) VALUES ($1, 'lic-c', 'commercial', $2)`, commercial, inAYear)
	configured := license.Load(license.Options{Key: enterprise, PublicKey: pubB64})
	csrv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Plans: plan.NewEnforcer(pool), License: configured}))
	t.Cleanup(csrv.Close)
	if code, body := callJSON(t, csrv, "lk-padmin", http.MethodPut, "/api/admin/license", map[string]any{"key": commercial}); code != http.StatusConflict || !strings.Contains(body["error"].(string), "MAVERICKS_LICENSE_KEY") {
		t.Fatalf("install over a configured key: %d %v", code, body)
	}
	if code, _ := callJSON(t, csrv, "lk-padmin", http.MethodDelete, "/api/admin/license", nil); code != http.StatusConflict {
		t.Fatalf("remove a configured key: %d", code)
	}
	if changed, err := ApplyStoredLicense(ctx, pool, configured); err != nil || changed || configured.Status().Edition != license.EditionEnterprise {
		t.Fatalf("a stored key read over a configured one: %v %v %+v", changed, err, configured.Status())
	}
}

// When a key's period is over the deployment and every tenant run the
// Community Edition at once (owner, 2026-11-22): the sync loop sees the
// edition drop while the gateway runs and moves the tenants, with no
// restart. In the Playground it moves nobody.
func TestTenantsTurnCommunityWhenTheKeyExpires(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Date(2026, 11, 24, 23, 59, 59, 0, time.UTC)
	tok, err := license.Sign(license.Claims{ID: "lic-exp", Edition: license.EditionEnterprise, Customer: "Acme Corp",
		Order: "ORD-1", Agreement: "PFA 2026-10", IssuedAt: expires.Add(-365 * 24 * time.Hour), ExpiresAt: expires}, priv)
	if err != nil {
		t.Fatal(err)
	}
	editionOf := func(name string) string {
		t.Helper()
		var p string
		if err := pool.QueryRow(ctx, `SELECT plan FROM core.customer WHERE name = $1`, name).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	run := func(playground bool, tenant string) string {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO core.customer (name, plan) VALUES ($1, 'enterprise')`, tenant); err != nil {
			t.Fatal(err)
		}
		var now atomic.Int64
		now.Store(expires.Add(-time.Hour).UnixNano())
		lic := license.Load(license.Options{Key: tok, PublicKey: license.EncodeKey(pub), Now: func() time.Time { return time.Unix(0, now.Load()) }})
		if lic.Edition() != license.EditionEnterprise {
			t.Fatalf("before expiry: %+v", lic.Status())
		}
		sctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			RunLicenseSync(sctx, pool, lic, playground, 10*time.Millisecond, logger.New("test"))
			close(done)
		}()
		time.Sleep(50 * time.Millisecond)
		if got := editionOf(tenant); got != "enterprise" {
			t.Fatalf("before expiry %s is on %s", tenant, got)
		}
		now.Store(expires.Add(time.Second).UnixNano()) // the key's period is over
		deadline := time.Now().Add(3 * time.Second)
		for editionOf(tenant) == "enterprise" && time.Now().Before(deadline) && !playground {
			time.Sleep(20 * time.Millisecond)
		}
		if playground {
			time.Sleep(100 * time.Millisecond)
		}
		cancel()
		<-done
		if lic.Edition() != license.EditionCommunity {
			t.Fatalf("after expiry the deployment runs %s", lic.Edition())
		}
		return editionOf(tenant)
	}
	if got := run(false, "Self-hosted Co"); got != "community" {
		t.Fatalf("after expiry a self-hosted tenant is on %s, want community", got)
	}
	if got := run(true, "Playground Co"); got != "enterprise" {
		t.Fatalf("in the Playground the tenant moved to %s", got)
	}
}
