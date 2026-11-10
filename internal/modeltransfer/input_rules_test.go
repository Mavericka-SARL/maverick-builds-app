package modeltransfer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A metric's input rules (migration 173) travel with a model: an import
// stores them, checked as a save checks them, and an export gives them back.
// The package's values are carried, not entered: one stored before the rule
// was added imports as it is, or an export would not import again.
func TestImportCarriesInputRules(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)

	pkg := formulaPackage(nil, nil)
	pkg.Metrics[0].InputRules = json.RawMessage(`{"min": 0, "whole": true}`)
	pkg.Facts = []Fact{{MetricID: "met-rev", DimMembers: json.RawMessage(`{"dim-region": "N", "dim-month": "2026-01"}`), Value: -3}}
	var modelID, revID string
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		modelID, revID, err = Import(ctx, tx, ImportRequest{ApplicationID: app, Package: pkg}, user)
		return err
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	var rules string
	var value float64
	if err := pool.QueryRow(ctx, `
		SELECT m.input_rules::text, fi.value::float8 FROM model.metric_def m
		JOIN runtime.fact_input fi ON fi.metric_id = m.id
		WHERE m.revision_id = $1::uuid AND m.name = 'rev'`, revID).Scan(&rules, &value); err != nil {
		t.Fatalf("read the imported metric: %v", err)
	}
	if rules != `{"min": 0, "whole": true}` || value != -3 {
		t.Errorf("imported rules %s, value %v; want the rules and -3 carried", rules, value)
	}

	out, err := CollectExport(ctx, pool, modelID, revID, "")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	for _, m := range out.Metrics {
		if m.Name == "rev" && string(m.InputRules) != `{"min": 0, "whole": true}` {
			t.Errorf("exported rules %s", m.InputRules)
		}
	}

	// Rules the metric cannot have are refused, as a save refuses them.
	bad := formulaPackage(nil, nil)
	bad.ModelName = "Bad rules"
	bad.Metrics[0].InputRules = json.RawMessage(`{"min": 5, "max": 1}`)
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := Import(ctx, tx, ImportRequest{ApplicationID: app, Package: bad}, user)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "above max") {
		t.Errorf("min above max imported: %v", err)
	}
}
