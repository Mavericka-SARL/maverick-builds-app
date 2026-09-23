package gateway

// The shared dimension importer behind both one-off wizard imports (new
// POST /api/import/dimension-members — one-offs used to 400 in the GRID
// importer) and saved-integration runs: property:<name> columns land in
// dimension_member.properties (merged, so re-imports converge), and rows
// with no code get one auto-generated from the label — stable across
// re-imports, suffixed on genuine collisions, hashed for labels with no
// ASCII (e.g. Cyrillic ERP exports, the live report).

import (
	"context"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

func TestImportDimensionMembersPropertiesAndAutoCodes(t *testing.T) {
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
	custID := q(`INSERT INTO core.customer (name) VALUES ('C') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, name, mode) VALUES ($1::uuid, 'App', 'planning') RETURNING id::text`, wsID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, appID)
	dimID := q(`INSERT INTO model.dimension_def (model_id, name) VALUES ($1::uuid, 'products') RETURNING id::text`, modelID)

	h := &handler{db: tenantdb.NewHandle(pool, nil), log: logger.New("test")}

	run := func(csvText string) (int, int) {
		t.Helper()
		cr := csv.NewReader(strings.NewReader(csvText))
		header, err := cr.Read()
		if err != nil {
			t.Fatalf("header: %v", err)
		}
		colIdx := map[string]int{}
		for i, c := range header {
			colIdx[strings.ToLower(strings.TrimSpace(c))] = i
		}
		imported, errs, fatal := h.importDimensionMembersCSV(ctx, dimID, cr, colIdx)
		if fatal != nil {
			t.Fatalf("import: %v", fatal)
		}
		return imported, errs
	}

	// No code column at all: codes auto-generate from labels; a property
	// column lands in properties.
	imported, errs := run("label,property:center\nMonitor Stand,Dragon\nАнгидак септ,Watson 3\n")
	if imported != 2 || errs != 0 {
		t.Fatalf("first import: %d/%d, want 2/0", imported, errs)
	}
	var code, props string
	if err := pool.QueryRow(ctx,
		`SELECT code, properties::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND label='Monitor Stand'`,
		dimID).Scan(&code, &props); err != nil {
		t.Fatalf("load member: %v", err)
	}
	if code != "MONITOR_STAND" {
		t.Errorf("auto code = %q, want MONITOR_STAND", code)
	}
	if !strings.Contains(props, `"center": "Dragon"`) && !strings.Contains(props, `"center":"Dragon"`) {
		t.Errorf("properties = %s, want center=Dragon", props)
	}

	// The mapped property must also be DECLARED (model.dimension_property),
	// not just written as a value — otherwise it shows as a column but is
	// unmanageable and invisible in the dimension's property definitions.
	var declared int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model.dimension_property WHERE dimension_id=$1::uuid AND name='center'`,
		dimID).Scan(&declared); err != nil {
		t.Fatalf("count declared props: %v", err)
	}
	if declared != 1 {
		t.Errorf("property 'center' declared %d times, want 1 (import must register it, once)", declared)
	}
	// Fully non-ASCII label: gets a stable hash code, not an empty one.
	var cyrCode string
	if err := pool.QueryRow(ctx,
		`SELECT code FROM model.dimension_member WHERE dimension_id=$1::uuid AND label='Ангидак септ'`,
		dimID).Scan(&cyrCode); err != nil {
		t.Fatalf("load cyrillic member: %v", err)
	}
	if !strings.HasPrefix(cyrCode, "M_") || len(cyrCode) < 6 {
		t.Errorf("cyrillic auto code = %q, want stable M_<hash>", cyrCode)
	}

	// Re-import with a changed property: same codes (idempotent), property
	// merged over the old value, count stays 2.
	imported, errs = run("label,property:center\nMonitor Stand,Ruchnaya\nАнгидак септ,Watson 3\n")
	if imported != 2 || errs != 0 {
		t.Fatalf("re-import: %d/%d, want 2/0", imported, errs)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_member WHERE dimension_id=$1::uuid`, dimID).Scan(&n)
	if n != 2 {
		t.Errorf("member count after re-import = %d, want 2 (no duplicates)", n)
	}
	_ = pool.QueryRow(ctx,
		`SELECT properties->>'center' FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='MONITOR_STAND'`,
		dimID).Scan(&props)
	if props != "Ruchnaya" {
		t.Errorf("re-imported property = %q, want Ruchnaya", props)
	}

	// A DIFFERENT label colliding to the same slug gets a suffix.
	imported, errs = run("label\nMonitor-Stand\n")
	if imported != 1 || errs != 0 {
		t.Fatalf("collision import: %d/%d, want 1/0", imported, errs)
	}
	var suffixed string
	if err := pool.QueryRow(ctx,
		`SELECT code FROM model.dimension_member WHERE dimension_id=$1::uuid AND label='Monitor-Stand'`,
		dimID).Scan(&suffixed); err != nil {
		t.Fatalf("load collided member: %v", err)
	}
	if suffixed != "MONITOR_STAND_2" {
		t.Errorf("collision code = %q, want MONITOR_STAND_2", suffixed)
	}

	// Explicit code + parent_code still work as before.
	imported, errs = run("code,label,parent_code\nCHILD1,Child One,MONITOR_STAND\n")
	if imported != 1 || errs != 0 {
		t.Fatalf("explicit import: %d/%d, want 1/0", imported, errs)
	}
	var parentOK bool
	_ = pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM model.dimension_member c
			JOIN model.dimension_member p ON p.id = c.parent_member_id
			WHERE c.dimension_id=$1::uuid AND c.code='CHILD1' AND p.code='MONITOR_STAND')
	`, dimID).Scan(&parentOK)
	if !parentOK {
		t.Error("explicit parent_code was not linked")
	}
}
