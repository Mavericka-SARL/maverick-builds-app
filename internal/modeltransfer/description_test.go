package modeltransfer

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// TestModelDescriptionTravels: what a model is for goes with it, exported
// from one tenant and imported into another.
func TestModelDescriptionTravels(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	srcModel, srcRev, err := importInto(ctx, pool, app, user, decodePackage(t, referencePackage))
	if err != nil {
		t.Fatal(err)
	}
	const desc = "Weekly order proposals per store."
	if _, err := pool.Exec(ctx, `UPDATE core.model SET description=$2 WHERE id=$1::uuid`, srcModel, desc); err != nil {
		t.Fatal(err)
	}
	exported, err := CollectExport(ctx, pool, srcModel, srcRev, "Imported")
	if err != nil {
		t.Fatal(err)
	}
	if exported.ModelDescription != desc {
		t.Errorf("exported model_description = %q, want %q", exported.ModelDescription, desc)
	}
	otherApp, otherUser := tenantTarget(t, ctx, pool, "Other Co")
	_, rev, err := importInto(ctx, pool, otherApp, otherUser, *exported)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := pool.QueryRow(ctx, `SELECT m.description FROM core.model m JOIN model.revision r ON r.model_id = m.id WHERE r.id=$1::uuid`, rev).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != desc {
		t.Errorf("imported model description = %q, want %q", got, desc)
	}
}

// TestModelDateFormatTravels: how a model's dates read goes with it; a
// package naming a format the engine does not know imports with none.
func TestModelDateFormatTravels(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	srcModel, srcRev, err := importInto(ctx, pool, app, user, decodePackage(t, referencePackage))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE core.model SET date_format='DD.MM.YYYY' WHERE id=$1::uuid`, srcModel); err != nil {
		t.Fatal(err)
	}
	exported, err := CollectExport(ctx, pool, srcModel, srcRev, "Imported")
	if err != nil {
		t.Fatal(err)
	}
	if exported.DateFormat != "DD.MM.YYYY" {
		t.Errorf("exported date_format = %q, want DD.MM.YYYY", exported.DateFormat)
	}
	read := func(rev string) string {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx, `SELECT m.date_format FROM core.model m JOIN model.revision r ON r.model_id = m.id WHERE r.id=$1::uuid`, rev).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	otherApp, otherUser := tenantTarget(t, ctx, pool, "Other Co")
	_, rev, err := importInto(ctx, pool, otherApp, otherUser, *exported)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(rev); got != "DD.MM.YYYY" {
		t.Errorf("imported date format = %q, want DD.MM.YYYY", got)
	}
	odd := *exported
	odd.DateFormat = "dd/mm/yy; DROP"
	thirdApp, thirdUser := tenantTarget(t, ctx, pool, "Third Co")
	_, rev, err = importInto(ctx, pool, thirdApp, thirdUser, odd)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(rev); got != "" {
		t.Errorf("unknown date format imported as %q, want none", got)
	}
}
