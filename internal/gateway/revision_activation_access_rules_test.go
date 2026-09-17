// Revision activation must carry identity.user_access_rule along: rules
// store raw member/metric UUIDs, every revision copy re-mints those UUIDs,
// and before this test's fix each activation quietly stranded every rule on
// the previous revision's rows — a "sees only Canada" user silently regained
// the whole world on the next promote. Same disease as the widget-ref and
// rate-operand copy bugs, one layer up.
package gateway

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

func TestRevisionActivationRemapsAccessRules(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name) VALUES ('Remap Co') RETURNING id::text`)
	appID := q(`INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'App', 'planning') RETURNING id::text`, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Model') RETURNING id::text`, appID)
	revA := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'A') RETURNING id::text`, modelID)
	revB := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'B') RETURNING id::text`, modelID)
	exec(`UPDATE core.model SET active_revision_id=$2::uuid WHERE id=$1::uuid`, modelID, revA)

	// The same geography exists in both revisions with fresh UUIDs — the
	// shape every revision copy produces. Member DE exists only in A, so its
	// rule has no counterpart and must be left alone (a dangling restriction
	// grants nothing; remapping wrongly could).
	seedGeo := func(revID string, codes []string) (dimID string, members map[string]string) {
		t.Helper()
		dimID = q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'geography', $2::uuid) RETURNING id::text`, modelID, revID)
		members = map[string]string{}
		for _, c := range codes {
			members[c] = q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $2) RETURNING id::text`, dimID, c)
		}
		return dimID, members
	}
	_, membersA := seedGeo(revA, []string{"CA", "US", "DE"})
	_, membersB := seedGeo(revB, []string{"CA", "US"})

	metricA := q(`INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id) VALUES ($1::uuid, 'revenue', true, 'sum', $2::uuid) RETURNING id::text`, modelID, revA)
	metricB := q(`INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id) VALUES ($1::uuid, 'revenue', true, 'sum', $2::uuid) RETURNING id::text`, modelID, revB)

	userID := q(`INSERT INTO identity.user (keycloak_sub, email) VALUES ('remap-user', 'remap@test.dev') RETURNING id::text`)
	exec(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`, userID, membersA["US"])
	exec(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`, userID, membersA["DE"])
	exec(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2, 'read')`, userID, metricA)

	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true}
	if _, err := h.activateRevision(ctx, revB); err != nil {
		t.Fatalf("activateRevision: %v", err)
	}

	refs := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT rule_type || ':' || access, ref_id FROM identity.user_access_rule WHERE user_id=$1::uuid`, userID)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, ref string
		_ = rows.Scan(&k, &ref)
		refs[k+":"+ref] = ref
	}
	assertRef := func(desc, want string) {
		t.Helper()
		found := false
		for _, ref := range refs {
			if ref == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: no rule points at %s; rules: %v", desc, want, refs)
		}
	}
	assertRef("US member rule remapped to revision B", membersB["US"])
	assertRef("DE rule (no counterpart in B) left on revision A", membersA["DE"])
	assertRef("metric rule remapped to revision B", metricB)
	for _, stale := range []string{membersA["US"], metricA} {
		for _, ref := range refs {
			if ref == stale {
				t.Errorf("a rule still points at the stranded revision-A row %s", stale)
			}
		}
	}
}
