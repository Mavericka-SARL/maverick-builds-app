package modeltransfer

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A package exported before property names were unique regardless of case
// (migration 170) can carry a developer's Segment next to an import's
// segment. It imports as one property — the first the package lists — with
// the members' values under it, the other spelling's value winning as the
// migration merged them, instead of failing on the unique index.
func TestImportMergesPropertiesDifferingOnlyInCase(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	const raw = `{
  "model_name": "Split",
  "dimensions": [
    {"id": "dim-region", "name": "Region",
     "typed_properties": [{"name": "Segment", "data_type": "text"}, {"name": "segment", "data_type": "text"}],
     "members": [
       {"id": "mem-n", "code": "N", "label": "North", "properties": {"Segment": "SMB", "segment": "ENT"}},
       {"id": "mem-s", "code": "S", "label": "South", "properties": {"segment": "MID"}}]}
  ]
}`
	var pkg Package
	if err := json.Unmarshal([]byte(raw), &pkg); err != nil {
		t.Fatalf("decode package: %v", err)
	}
	var revID string
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		_, revID, err = Import(ctx, tx, ImportRequest{ApplicationID: app, Package: pkg}, user)
		return err
	}); err != nil {
		t.Fatalf("import a package with Segment and segment: %v", err)
	}
	const byDim = `dimension_id IN (SELECT id FROM model.dimension_def WHERE revision_id=$1::uuid)`
	if got := texts(t, ctx, pool, `SELECT name FROM model.dimension_property WHERE `+byDim, revID); len(got) != 1 || got[0] != "Segment" {
		t.Errorf("declarations = %v, want [Segment]", got)
	}
	got := texts(t, ctx, pool, `SELECT code || '=' || properties::text FROM model.dimension_member WHERE `+byDim+` ORDER BY code`, revID)
	want := []string{`N={"Segment": "ENT"}`, `S={"Segment": "MID"}`}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("members = %v, want %v", got, want)
	}
}
