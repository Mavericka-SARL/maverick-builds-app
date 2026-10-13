package plan

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// Off the Playground, the Community Edition loses the 100 MB the migrations
// seed for the Playground, once; an allowance an administrator set stays.
func TestReleasePlaygroundAllowance(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	community := func() Plan {
		t.Helper()
		p, err := Get(ctx, pool, "community")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := community(); p.Limits.MaxStorageMB != playgroundStorageMB || p.LimitNote != playgroundNote || p.Description != playgroundDescription {
		t.Fatalf("seeded community = %+v, want the Playground's allowance as this package quotes it", p)
	}
	released, err := ReleasePlaygroundAllowance(ctx, pool)
	if err != nil || !released {
		t.Fatalf("release: %v %v", released, err)
	}
	p := community()
	if p.Limits.Any() || p.LimitNote != "" || p.Description != SelfHostedCommunityDescription || !p.SelfService {
		t.Fatalf("released community = %+v", p)
	}
	if released, err := ReleasePlaygroundAllowance(ctx, pool); err != nil || released {
		t.Fatalf("second release: %v %v, want nothing to do", released, err)
	}

	// An administrator's own 100 MB is theirs: a different note, or none.
	p.Limits.MaxStorageMB = 100
	p.LimitNote = "Ask IT for more room."
	if _, err := Update(ctx, pool, p); err != nil {
		t.Fatal(err)
	}
	if released, err := ReleasePlaygroundAllowance(ctx, pool); err != nil || released {
		t.Fatalf("release over an administrator's limit: %v %v", released, err)
	}
	if got := community(); got.Limits.MaxStorageMB != 100 {
		t.Fatalf("administrator's limit lost: %+v", got)
	}
}
