package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/pkg/logger"

	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A model link into a table reads the source's whole rows at every run. A
// table that keeps no dimension added them again as new records each time —
// 2 source rows, 4 records after two runs (2026-11-09) — whatever the import
// mode. Such a link now replaces by a field, or does not run.
func TestModelLinkIntoATableReplacesByAField(t *testing.T) {
	pool := testdb.New(t, migrationfs.FS, ".")
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return id
	}
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('FormCo','enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App','planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, modelID)
	form := q(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid,$2::uuid,'plan','Plan',
		jsonb_build_array(jsonb_build_object('name','region','label','Region','type','text'),
			jsonb_build_object('name','amount','label','Amount','type','number'))) RETURNING id::text`, modelID, revID)
	dev := q(`INSERT INTO identity.user (keycloak_sub, email, customer_id) VALUES ('dev','dev@t.co',$1::uuid) RETURNING id::text`, custID)
	c := &integration.DBCommitter{Pool: pool, Log: logger.New("test")}
	def := &integration.Definition{ModelID: modelID, RevisionID: revID, Config: &integration.Config{
		Direction: integration.DirectionPull, Protocol: integration.ProtocolModel, ImportMode: integration.ModeFullReload,
		TargetType: integration.TargetForm, TargetID: form}}
	header := []string{"region", "amount"}
	rows := [][]string{{"US", "10"}, {"EU", "5"}}
	count := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, form).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// No field to replace by: refused, nothing written.
	if _, err := c.CommitPull(ctx, def, header, rows, integration.PullOptions{RunBy: dev}); !errors.Is(err, integration.ErrModelLinkTableNeedsKey) {
		t.Fatalf("an unkeyed model link into a table: %v, want ErrModelLinkTableNeedsKey", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("the refused run created %d record(s)", n)
	}

	// Replacing by region, two runs leave the source's two rows.
	def.Config.ReplaceBy = "region"
	for i := range 2 {
		if _, err := c.CommitPull(ctx, def, header, rows, integration.PullOptions{RunBy: dev}); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	if n := count(); n != 2 {
		t.Errorf("records after two runs of 2 source rows = %d, want 2", n)
	}

	// Another connector into the table keeps adding: a REST pull's file of
	// new rows is not the source's whole rows.
	def.Config.Protocol, def.Config.ReplaceBy = integration.ProtocolHTTPS, ""
	if _, err := c.CommitPull(ctx, def, header, rows, integration.PullOptions{RunBy: dev}); err != nil {
		t.Fatalf("a REST pull: %v", err)
	}
	if n := count(); n != 4 {
		t.Errorf("records after a REST pull adds 2 = %d, want 4", n)
	}
}
