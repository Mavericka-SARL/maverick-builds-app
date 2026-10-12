package migrations_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/migrations"
)

// TestCommunityNote132 applies migrations 001–131, then 132: the seeded
// Community note now says "Enterprise Edition", as every other word people
// read does, and a note a platform administrator wrote is kept.
func TestCommunityNote132(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("note132_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)

	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	apply := func(f string) {
		t.Helper()
		sql, err := fs.ReadFile(migrations.FS, f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	for _, f := range files {
		if f >= "132" {
			break
		}
		apply(f)
	}
	note := func() string {
		t.Helper()
		var n string
		if err := conn.QueryRow(ctx, `SELECT limit_note FROM platform.plan WHERE key = 'community'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := note(); !strings.HasSuffix(n, "the Commercial or Enterprise edition.") {
		t.Fatalf("before 132 the seeded note should be 129's wording, got %q", n)
	}

	apply("132_community_note_edition.sql")
	n := note()
	if !strings.HasSuffix(n, "the Commercial or Enterprise Edition.") ||
		!strings.Contains(n, "free for your company's internal use") {
		t.Fatalf("132 did not replace the seeded note: %q", n)
	}

	// A note the platform administrator wrote is theirs: re-running 132 over
	// it changes nothing.
	const own = "Ask us about more space."
	if _, err := conn.Exec(ctx, `UPDATE platform.plan SET limit_note = $1 WHERE key = 'community'`, own); err != nil {
		t.Fatal(err)
	}
	apply("132_community_note_edition.sql")
	if n := note(); n != own {
		t.Fatalf("132 overwrote an administrator's note: %q", n)
	}
}
