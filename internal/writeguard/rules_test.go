package writeguard

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lineageFixture is two revisions of one model whose dimensions, members
// and metrics are linked by lineage_id the way revision duplication links
// them, plus helpers to add rows and read the resolver's answer.
type lineageFixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	f        fixture
	revA     string
	revB     string
	lineages map[string]string // key -> lineage id, minted on first use
}

func newLineageFixture(t *testing.T, pool *pgxpool.Pool) *lineageFixture {
	lf := &lineageFixture{t: t, pool: pool, f: seedFixture(t, pool), lineages: map[string]string{}}
	lf.revA = lf.f.revisionID
	lf.revB = lf.q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Next') RETURNING id::text`, lf.f.modelID)
	return lf
}

func (lf *lineageFixture) q(sql string, args ...any) string {
	lf.t.Helper()
	var id string
	if err := lf.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		lf.t.Fatalf("query %q: %v", sql, err)
	}
	return id
}

func (lf *lineageFixture) exec(sql string, args ...any) {
	lf.t.Helper()
	if _, err := lf.pool.Exec(context.Background(), sql, args...); err != nil {
		lf.t.Fatalf("exec %q: %v", sql, err)
	}
}

// lin returns the lineage for key, minting it on first use; "" is a fresh
// one every time (a new row with no copies).
func (lf *lineageFixture) lin(key string) string {
	if key == "" {
		return uuid.NewString()
	}
	if _, ok := lf.lineages[key]; !ok {
		lf.lineages[key] = uuid.NewString()
	}
	return lf.lineages[key]
}

// dim adds a dimension to rev ("" = revision-less) with the given lineage key.
func (lf *lineageFixture) dim(rev, name, key string) string {
	if rev == "" {
		return lf.q(`INSERT INTO model.dimension_def (model_id, name, lineage_id) VALUES ($1::uuid, $2, $3::uuid) RETURNING id::text`,
			lf.f.modelID, name, lf.lin(key))
	}
	return lf.q(`INSERT INTO model.dimension_def (model_id, name, revision_id, lineage_id) VALUES ($1::uuid, $2, $3::uuid, $4::uuid) RETURNING id::text`,
		lf.f.modelID, name, rev, lf.lin(key))
}

func (lf *lineageFixture) member(dimID, code, key string) string {
	return lf.q(`INSERT INTO model.dimension_member (dimension_id, code, label, lineage_id) VALUES ($1::uuid, $2, $2, $3::uuid) RETURNING id::text`,
		dimID, code, lf.lin(key))
}

func (lf *lineageFixture) metric(rev, name, key string) string {
	return lf.q(`INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id, lineage_id) VALUES ($1::uuid, $2, true, 'sum', $3::uuid, $4::uuid) RETURNING id::text`,
		lf.f.modelID, name, rev, lf.lin(key))
}

// setRules replaces the user's rules the way every rule writer does.
func (lf *lineageFixture) setRules(rules ...RuleInput) {
	lf.t.Helper()
	ctx := context.Background()
	tx, err := lf.pool.Begin(ctx)
	if err != nil {
		lf.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := ReplaceUserRules(ctx, tx, lf.f.userID, rules); err != nil {
		lf.t.Fatalf("ReplaceUserRules: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		lf.t.Fatal(err)
	}
}

// check asserts RulesForRevision(rev) is exactly want (type:ref -> access).
func (lf *lineageFixture) check(label, rev string, want map[string]string) {
	lf.t.Helper()
	rules, err := RulesForRevision(context.Background(), lf.pool, lf.f.userID, rev)
	if err != nil {
		lf.t.Fatalf("%s: RulesForRevision(%s): %v", label, rev, err)
	}
	got := map[string]string{}
	for _, r := range rules {
		got[r.Type+":"+r.RefID] = r.Access
	}
	if len(got) != len(want) {
		lf.t.Errorf("%s: revision %s: got %v, want %v", label, rev, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			lf.t.Errorf("%s: revision %s: %s = %q, want %q (all: %v)", label, rev, k, got[k], v, got)
		}
	}
}

// TestRulesForRevisionResolvesByLineage: a rule's stored ref_id lives in
// one revision; RulesForRevision and the single-id checks apply it to the
// row of the same LINEAGE in whichever revision is read — whatever that
// row's dimension, code or name is there — drop it where that revision has
// no row of the lineage, keep the stricter access when two rules land on
// one row, count revision-less rows in every revision, resolve a rule
// stored without a lineage through its ref_id, and pass non-revision rule
// types through.
func TestRulesForRevisionResolvesByLineage(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()
	ctx := context.Background()
	lf := newLineageFixture(t, pool)

	geoA := lf.dim(lf.revA, "Geography", "geo")
	geoB := lf.dim(lf.revB, "Zone", "geo") // renamed in B: same lineage
	caA, caB := lf.member(geoA, "CA", "ca"), lf.member(geoB, "CA", "ca")
	usA, usB := lf.member(geoA, "US", "us"), lf.member(geoB, "USA", "us") // code renamed in B
	deA := lf.member(geoA, "DE", "de")
	deB := lf.member(geoB, "DE", "") // DE deleted and re-added in B: a new lineage
	chanDim := lf.dim("", "channel", "chan")
	web := lf.member(chanDim, "WEB", "web")
	revenueA, revenueB := lf.metric(lf.revA, "revenue", "rev"), lf.metric(lf.revB, "sales", "rev") // renamed in B

	lf.setRules(
		RuleInput{"dimension_member", usB, "hidden"},
		RuleInput{"dimension_member", caA, "read"},
		RuleInput{"dimension_member", caB, "write"},
		RuleInput{"dimension_member", deA, "hidden"},
		RuleInput{"dimension_member", web, "hidden"},
		RuleInput{"dimension_member", "not-a-member-id", "hidden"},
		RuleInput{"metric", revenueB, "hidden"},
		RuleInput{"button", "btn-1", "write"},
	)
	var withLineage, buttonLineage int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE ref_lineage_id IS NOT NULL),
	                                     count(*) FILTER (WHERE rule_type = 'button' AND ref_lineage_id IS NOT NULL)
	                              FROM identity.user_access_rule WHERE user_id = $1::uuid`, lf.f.userID).Scan(&withLineage, &buttonLineage); err != nil {
		t.Fatal(err)
	}
	if withLineage != 6 || buttonLineage != 0 {
		t.Errorf("ReplaceUserRules stored %d rules with a lineage (%d buttons), want 6 (0)", withLineage, buttonLineage)
	}

	lf.check("lineage", lf.revA, map[string]string{
		"dimension_member:" + usA: "hidden",
		"dimension_member:" + caA: "read",
		"dimension_member:" + deA: "hidden",
		"dimension_member:" + web: "hidden",
		"metric:" + revenueA:      "hidden",
		"button:btn-1":            "write",
	})
	lf.check("lineage", lf.revB, map[string]string{
		"dimension_member:" + usB: "hidden",
		"dimension_member:" + caB: "read", // read (from A) beats write (on B)
		"dimension_member:" + web: "hidden",
		"metric:" + revenueB:      "hidden",
		"button:btn-1":            "write",
		// deB is a new lineage: unrestricted.
	})
	if rules, err := RulesForRevision(ctx, pool, lf.f.userID, ""); err != nil || len(rules) != 8 {
		t.Errorf("no revision: %d rules (err %v), want the 8 stored", len(rules), err)
	}
	if _, err := RulesForRevision(ctx, pool, lf.f.userID, "not-a-revision"); err == nil {
		t.Error("a malformed revision id resolved; want an error (callers fail closed)")
	}

	// A rule stored without a lineage resolves through the row its ref_id
	// points at.
	lf.exec(`UPDATE identity.user_access_rule SET ref_lineage_id = NULL WHERE user_id = $1::uuid AND ref_id = $2`, lf.f.userID, deA)
	lf.exec(`UPDATE identity.user_access_rule SET ref_lineage_id = NULL WHERE user_id = $1::uuid AND ref_id = $2`, lf.f.userID, usB)
	lf.check("no stored lineage", lf.revA, map[string]string{
		"dimension_member:" + usA: "hidden",
		"dimension_member:" + caA: "read",
		"dimension_member:" + deA: "hidden",
		"dimension_member:" + web: "hidden",
		"metric:" + revenueA:      "hidden",
		"button:btn-1":            "write",
	})

	if got, err := HiddenAccess(ctx, pool, lf.f.userID, usA); err != nil || got != "hidden" {
		t.Errorf("HiddenAccess(US in A) = %q, %v; want hidden (rule is on B's USA)", got, err)
	}
	if got, err := HiddenAccess(ctx, pool, lf.f.userID, caB); err != nil || got != "read" {
		t.Errorf("HiddenAccess(CA in B) = %q, %v; want read (stricter of read and write)", got, err)
	}
	if got, err := HiddenAccess(ctx, pool, lf.f.userID, deB); err != nil || got != "" {
		t.Errorf("HiddenAccess(re-added DE in B) = %q, %v; want unrestricted (new lineage)", got, err)
	}
	if got, err := MetricAccess(ctx, pool, lf.f.userID, revenueA); err != nil || got != "hidden" {
		t.Errorf("MetricAccess(revenue in A) = %q, %v; want hidden (rule is on B's sales)", got, err)
	}
	if reason, err := CheckWrite(ctx, pool, lf.f.modelID, lf.revA, lf.f.userID, []string{usA}); err != nil || reason == "" {
		t.Errorf("CheckWrite(US in A) = %q, %v; want refused", reason, err)
	}
}

// TestRuleLineageSurvivesRenameAndDelete: renames of the dimension, the
// member code and the metric never touch lineage, so a rule keeps applying
// in every revision. Deleting the row a rule points at leaves the rule's
// lineage in place — the old revision's copy stays restricted, also after
// an admin re-saves the listed rules — while a row re-added under the same
// code or name is a new lineage and is unrestricted until a rule is set on
// it.
func TestRuleLineageSurvivesRenameAndDelete(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()
	ctx := context.Background()
	lf := newLineageFixture(t, pool)

	dimA, dimB := lf.dim(lf.revA, "region", "region"), lf.dim(lf.revB, "region", "region")
	usA, usB := lf.member(dimA, "US", "us"), lf.member(dimB, "US", "us")
	revenueA, revenueB := lf.metric(lf.revA, "revenue", "rev"), lf.metric(lf.revB, "revenue", "rev")
	stored := []RuleInput{{"dimension_member", usB, "hidden"}, {"metric", revenueB, "hidden"}}
	lf.setRules(stored...)

	hidden := func(label string, rev, member, metric string) {
		t.Helper()
		dim, met, err := RuleMaps(ctx, pool, lf.f.userID, rev)
		if err != nil {
			t.Fatal(err)
		}
		if dim[member] != "hidden" || met[metric] != "hidden" {
			t.Errorf("%s: revision %s rules %v / %v, want member %s and metric %s hidden", label, rev, dim, met, member, metric)
		}
		if a, err := HiddenAccess(ctx, pool, lf.f.userID, member); err != nil || a != "hidden" {
			t.Errorf("%s: HiddenAccess(%s) = %q, %v; want hidden", label, member, a, err)
		}
		if a, err := MetricAccess(ctx, pool, lf.f.userID, metric); err != nil || a != "hidden" {
			t.Errorf("%s: MetricAccess(%s) = %q, %v; want hidden", label, metric, a, err)
		}
	}
	hidden("baseline", lf.revA, usA, revenueA)
	hidden("baseline", lf.revB, usB, revenueB)

	lf.exec(`UPDATE model.dimension_def SET name = 'Geo' WHERE id = $1::uuid`, dimB)
	lf.exec(`UPDATE model.dimension_member SET code = 'USA' WHERE id = $1::uuid`, usB)
	lf.exec(`UPDATE model.metric_def SET name = 'sales' WHERE id = $1::uuid`, revenueB)
	hidden("renamed in B", lf.revA, usA, revenueA)
	hidden("renamed in B", lf.revB, usB, revenueB)

	// Rules written afresh after the renames resolve the same way.
	lf.setRules()
	lf.setRules(stored...)
	hidden("rules written after the renames", lf.revA, usA, revenueA)

	// The rows the rules point at are deleted, then re-added.
	lf.exec(`DELETE FROM model.dimension_member WHERE id = $1::uuid`, usB)
	lf.exec(`DELETE FROM model.metric_def WHERE id = $1::uuid`, revenueB)
	hidden("deleted in B", lf.revA, usA, revenueA)
	usB2 := lf.member(dimB, "US", "")
	revenueB2 := lf.metric(lf.revB, "revenue", "")
	lf.check("re-added in B", lf.revB, map[string]string{})
	if a, err := HiddenAccess(ctx, pool, lf.f.userID, usB2); err != nil || a != "" {
		t.Errorf("HiddenAccess(re-added US) = %q, %v; want unrestricted (new lineage)", a, err)
	}
	if a, err := MetricAccess(ctx, pool, lf.f.userID, revenueB2); err != nil || a != "" {
		t.Errorf("MetricAccess(re-added revenue) = %q, %v; want unrestricted (new lineage)", a, err)
	}

	// An admin re-saving the listed rules (still pointing at the deleted
	// rows) keeps their lineage, so A stays restricted.
	lf.setRules(stored...)
	hidden("re-saved after the delete", lf.revA, usA, revenueA)

	// Setting a rule on the re-added rows restricts them too.
	lf.setRules(append(stored, RuleInput{"dimension_member", usB2, "hidden"}, RuleInput{"metric", revenueB2, "hidden"})...)
	hidden("rule on the re-added rows", lf.revB, usB2, revenueB2)
	hidden("rule on the re-added rows", lf.revA, usA, revenueA)
}

// TestReplaceUserMemberRulesInRevisionScope: the scoped replace the AI's
// set_user_access_rules uses touches only the member rules that resolve (by
// lineage) in the given revision — including a rule stored against another
// revision's copy of a row in it, and a revision-less member — and keeps
// every rule the caller cannot name there: a member gone from that revision
// (it still restricts the old revision's copy), metric and button rules.
func TestReplaceUserMemberRulesInRevisionScope(t *testing.T) {
	pool, cleanup := setupWriteguardDB(t)
	defer cleanup()
	ctx := context.Background()
	lf := newLineageFixture(t, pool)

	dimA, dimB := lf.dim(lf.revA, "region", "region"), lf.dim(lf.revB, "region", "region")
	usA := lf.member(dimA, "US", "us") // deleted from B: no row of its lineage there
	ukA, ukB := lf.member(dimA, "UK", "uk"), lf.member(dimB, "UK", "uk")
	caB := lf.member(dimB, "CA", "ca")
	shared := lf.member(lf.dim("", "channel", "channel"), "WEB", "web")
	costB := lf.metric(lf.revB, "cost", "cost")
	button := uuid.NewString()
	lf.setRules(
		RuleInput{"dimension_member", usA, "hidden"},
		RuleInput{"dimension_member", ukA, "hidden"}, // stored against A's copy
		RuleInput{"dimension_member", shared, "read"},
		RuleInput{"metric", costB, "hidden"},
		RuleInput{"button", button, "hidden"},
	)

	replace := func(rules ...RuleInput) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := ReplaceUserMemberRulesInRevision(ctx, tx, lf.f.userID, lf.revB, rules); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err := replace(RuleInput{"metric", costB, "read"}); err == nil {
		t.Fatal("a metric rule must be refused by the member-only replace")
	}
	if err := replace(RuleInput{"dimension_member", caB, "read"}); err != nil {
		t.Fatalf("scoped replace: %v", err)
	}

	lf.check("A after the scoped replace", lf.revA, map[string]string{
		"dimension_member:" + usA: "hidden", // kept: not in B
		"button:" + button:        "hidden",
	})
	lf.check("B after the scoped replace", lf.revB, map[string]string{
		"dimension_member:" + caB: "read",
		"metric:" + costB:         "hidden",
		"button:" + button:        "hidden",
	})
	if a, err := HiddenAccess(ctx, pool, lf.f.userID, ukB); err != nil || a != "" {
		t.Errorf("UK (in B, not re-listed) = %q, %v; want unrestricted", a, err)
	}
}
