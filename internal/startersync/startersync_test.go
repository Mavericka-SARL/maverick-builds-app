package startersync

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/starter"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

var day = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
}

func (f fixture) one(sql string, args ...any) string {
	f.t.Helper()
	var out string
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&out); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return out
}

func (f fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// signedUp is a tenant as sign-up left it: its workspace, the person who
// signed up (and the audit event that says so), and its "Getting started"
// application, empty unless withApp is false (then there is none).
func (f fixture) signedUp(name string, withApp bool) (cust, user, app string) {
	cust = f.one(`INSERT INTO core.customer (name, plan) VALUES ($1, 'community') RETURNING id::text`, name)
	ws := f.one(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	user = f.one(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1 || '@example.test', 'Owner', $2::uuid) RETURNING id::text`,
		"owner-"+strings.ToLower(strings.ReplaceAll(name, " ", "-")), cust)
	f.exec(`INSERT INTO audit.audit_event (category, event_type, actor_user_id, actor_role, resource_type, resource_id)
	        VALUES ('admin', 'tenant.signed_up', $1::uuid, 'signup', 'tenant', $2)`, user, cust)
	if withApp {
		app = f.one(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, $3, 'planning') RETURNING id::text`, cust, ws, AppName)
	}
	return
}

// importInto imports pkg into app the way an older sign-up did: no record.
func (f fixture) importInto(app, user string, pkg modeltransfer.Package) (modelID, revisionID string) {
	f.t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	modelID, revisionID, err = modeltransfer.Import(f.ctx, tx, modeltransfer.ImportRequest{ApplicationID: app, Package: pkg}, user)
	if err != nil {
		_ = tx.Rollback(f.ctx)
		f.t.Fatalf("import %q: %v", pkg.ModelName, err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f fixture) sync(cust string, starters []starter.Starter) []Change {
	f.t.Helper()
	got, err := Sync(f.ctx, f.pool, cust, starters, day)
	if err != nil {
		f.t.Fatalf("sync: %v", err)
	}
	return got
}

// withText is the starter with its first text widget saying text — content
// the starter had before, or will have after a change.
func withText(s starter.Starter, text string) starter.Starter {
	pkg := s.Package
	pkg.Dashboards = append([]modeltransfer.Dashboard(nil), pkg.Dashboards...)
	for i := range pkg.Dashboards {
		ws := append([]modeltransfer.Widget(nil), pkg.Dashboards[i].Widgets...)
		for j := range ws {
			if ws[j].WidgetType == "text" {
				ws[j].Content = &text
				pkg.Dashboards[i].Widgets = ws
				s.Package = pkg
				return s
			}
		}
	}
	panic("starter has no text widget")
}

// TestSyncBringsAnEarlierTenantUpToDate: a tenant that signed up when sign-up
// gave only the tour, which has since changed. The guides it never had are
// installed; the tour gets the current content as a new live revision, with
// the old one kept and the tenant's access rule and role grant carried over.
func TestSyncBringsAnEarlierTenantUpToDate(t *testing.T) {
	ctx := context.Background()
	f := fixture{t: t, ctx: ctx, pool: testdb.New(t, migrationfs.FS, ".")}
	current := starter.Starters()
	cust, user, app := f.signedUp("Early Co", true)
	tourID, oldRev := f.importInto(app, user, withText(current[0], "OLD TOUR TEXT").Package)

	// What the tenant set on the old tour: a Read rule on a team, and a role
	// granted the first dashboard.
	member := f.one(`SELECT m.id::text FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
	                 WHERE d.revision_id = $1::uuid AND d.name = 'team' AND m.code <> 'COMPANY' ORDER BY m.sort_order LIMIT 1`, oldRev)
	lineage := f.one(`SELECT lineage_id::text FROM model.dimension_member WHERE id = $1::uuid`, member)
	f.exec(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access, ref_lineage_id)
	        VALUES ($1::uuid, 'dimension_member', $2, 'read', $3::uuid)`, user, member, lineage)
	ws := f.one(`SELECT workspace_id::text FROM core.application WHERE id = $1::uuid`, app)
	role := f.one(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Planners') RETURNING id::text`, ws)
	dash := f.one(`SELECT id::text FROM model.dashboard_def WHERE revision_id = $1::uuid ORDER BY name LIMIT 1`, oldRev)
	dashName := f.one(`SELECT name FROM model.dashboard_def WHERE id = $1::uuid`, dash)
	f.exec(`INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid)`, role, dash)

	changes := f.sync(cust, current)
	if len(changes) != 4 {
		t.Fatalf("changes = %+v, want the tour updated and three guides installed", changes)
	}

	// Every starter is recorded with today's content.
	for _, s := range current {
		var hash string
		if err := f.pool.QueryRow(ctx, `SELECT content_hash FROM core.starter_model WHERE customer_id=$1::uuid AND starter_key=$2`, cust, s.Key).Scan(&hash); err != nil {
			t.Fatalf("record for %s: %v", s.Key, err)
		}
		if hash != Hash(s.Package) {
			t.Errorf("%s recorded with an old hash", s.Key)
		}
	}
	// The guides went into the same application; the tour kept its model.
	if n := f.one(`SELECT count(*)::text FROM core.model WHERE application_id = $1::uuid`, app); n != "4" {
		t.Errorf("models in %s = %s, want 4", AppName, n)
	}
	newRev := f.one(`SELECT active_revision_id::text FROM core.model WHERE id = $1::uuid`, tourID)
	if newRev == oldRev {
		t.Fatal("tour's live revision unchanged")
	}
	if name := f.one(`SELECT name FROM model.revision WHERE id = $1::uuid`, newRev); name != "Updated 2026-10-05" {
		t.Errorf("new revision named %q", name)
	}
	if n := f.one(`SELECT count(*)::text FROM model.revision WHERE model_id = $1::uuid`, tourID); n != "2" {
		t.Errorf("tour revisions = %s, want the old one kept beside the new", n)
	}
	if n := f.one(`SELECT count(*)::text FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id = w.dashboard_id
	               WHERE d.revision_id = $1::uuid AND w.content = 'OLD TOUR TEXT'`, newRev); n != "0" {
		t.Error("the live tour still shows the old text")
	}
	// The rule follows its team into the new revision; the grant follows its dashboard.
	newMember := f.one(`SELECT m.id::text FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
	                    WHERE d.revision_id = $1::uuid AND m.lineage_id = $2::uuid`, newRev, lineage)
	if ref := f.one(`SELECT ref_id FROM identity.user_access_rule WHERE user_id = $1::uuid`, user); ref != newMember {
		t.Errorf("access rule points at %s, want the new revision's member %s", ref, newMember)
	}
	if n := f.one(`SELECT count(*)::text FROM identity.business_role_dashboard brd JOIN model.dashboard_def d ON d.id = brd.dashboard_id
	               WHERE brd.role_id = $1::uuid AND d.revision_id = $2::uuid AND d.name = $3`, role, newRev, dashName); n != "1" {
		t.Error("the role lost its dashboard in the new revision")
	}
	// The tour's calculated values are computed once Run recalculates.
	Recalculate(ctx, f.pool, zerolog.Nop(), changes)
	if n := f.one(`SELECT count(*)::text FROM runtime.calc_result WHERE revision_id = $1::uuid`, newRev); n == "0" {
		t.Error("no calculated values in the new tour revision")
	}

	// Run again: nothing to do.
	if again := f.sync(cust, current); len(again) != 0 {
		t.Errorf("second sync changed %+v", again)
	}
}

// TestSyncLeavesWhatTheTenantChose: a starter model deleted is not put back,
// and one whose live revision the tenant changed is not touched by new
// content — while another starter's new content still arrives.
func TestSyncLeavesWhatTheTenantChose(t *testing.T) {
	ctx := context.Background()
	f := fixture{t: t, ctx: ctx, pool: testdb.New(t, migrationfs.FS, ".")}
	current := starter.Starters()
	cust, _, _ := f.signedUp("Chooser Co", true)
	if got := f.sync(cust, current); len(got) != 4 {
		t.Fatalf("first sync installed %d, want 4", len(got))
	}
	model := func(key string) string {
		return f.one(`SELECT COALESCE(model_id::text, '') FROM core.starter_model WHERE customer_id=$1::uuid AND starter_key=$2`, cust, key)
	}

	devGuide := model("developer_guide")
	f.exec(`DELETE FROM core.model WHERE id = $1::uuid`, devGuide)
	baGuide := model("business_admin_guide")
	own := f.one(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Our own') RETURNING id::text`, baGuide)
	f.exec(`UPDATE core.model SET active_revision_id = $2::uuid WHERE id = $1::uuid`, baGuide, own)

	// New content for every starter.
	next := make([]starter.Starter, len(current))
	for i, s := range current {
		next[i] = withText(s, "NEXT TEXT")
	}
	got := f.sync(cust, next)
	if len(got) != 2 {
		t.Fatalf("changes = %+v, want the tour and the tenant admin guide only", got)
	}
	for _, ch := range got {
		if ch.StarterKey != starter.TourKey && ch.StarterKey != "tenant_admin_guide" {
			t.Errorf("changed %s", ch.StarterKey)
		}
	}
	if model("developer_guide") != "" {
		t.Error("the deleted developer guide came back")
	}
	if live := f.one(`SELECT active_revision_id::text FROM core.model WHERE id = $1::uuid`, baGuide); live != own {
		t.Error("the business admin guide's own live revision was replaced")
	}
}

// TestSyncRemakesTheApplication: a tenant that deleted its "Getting started"
// application gets it again, with every starter and the landing one (the
// developer guide) as default.
func TestSyncRemakesTheApplication(t *testing.T) {
	ctx := context.Background()
	f := fixture{t: t, ctx: ctx, pool: testdb.New(t, migrationfs.FS, ".")}
	cust, _, _ := f.signedUp("No App Co", false)
	if got := f.sync(cust, starter.Starters()); len(got) != 4 {
		t.Fatalf("installed %d, want 4", len(got))
	}
	landing := f.one(`SELECT model_id::text FROM core.starter_model WHERE customer_id=$1::uuid AND starter_key=$2`, cust, starter.LandingKey)
	if def := f.one(`SELECT COALESCE(default_model_id::text, '') FROM core.application WHERE customer_id=$1::uuid AND name=$2`, cust, AppName); def != landing {
		t.Errorf("default model %q, want the landing model %s", def, landing)
	}
}

// TestRunOnlyTouchesSelfServiceTenantsOnce: Run syncs the tenants that
// signed up for themselves, not others; two replicas starting together
// install each starter once.
func TestRunOnlyTouchesSelfServiceTenantsOnce(t *testing.T) {
	ctx := context.Background()
	f := fixture{t: t, ctx: ctx, pool: testdb.New(t, migrationfs.FS, ".")}
	cust, _, _ := f.signedUp("Racing Co", true)
	other := f.one(`INSERT INTO core.customer (name, plan) VALUES ('Enterprise Co', 'enterprise') RETURNING id::text`)
	f.exec(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default')`, other)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); Run(ctx, f.pool, zerolog.Nop()) }()
	}
	wg.Wait()
	if n := f.one(`SELECT count(*)::text FROM core.model m JOIN core.application a ON a.id = m.application_id WHERE a.customer_id = $1::uuid`, cust); n != "4" {
		t.Errorf("signed-up tenant has %s models, want 4", n)
	}
	if n := f.one(`SELECT count(*)::text FROM core.application WHERE customer_id = $1::uuid`, other); n != "0" {
		t.Errorf("a tenant that did not sign up got %s applications", n)
	}
}
