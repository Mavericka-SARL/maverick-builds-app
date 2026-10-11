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

// TestCommunityNote129 applies migrations 001–128, then 129: the seeded
// Community note now says self-hosting is free for a company's internal use,
// as the licence does, and a note a platform administrator wrote is kept.
func TestCommunityNote129(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("note129_%d", time.Now().UnixNano())
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
		if f >= "129" {
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
	if n := note(); !strings.Contains(n, "for non-commercial use, under a commercial licence otherwise") {
		t.Fatalf("before 129 the seeded note should be the old wording, got %q", n)
	}

	apply("129_community_note_licence.sql")
	n := note()
	if !strings.Contains(n, "free for your company's internal use") ||
		strings.Contains(n, "non-commercial use, under a commercial licence otherwise") {
		t.Fatalf("129 did not replace the seeded note: %q", n)
	}

	// A note the platform administrator wrote is theirs: re-running 129 over
	// it changes nothing.
	const own = "Ask us about more space."
	if _, err := conn.Exec(ctx, `UPDATE platform.plan SET limit_note = $1 WHERE key = 'community'`, own); err != nil {
		t.Fatal(err)
	}
	apply("129_community_note_licence.sql")
	if n := note(); n != own {
		t.Fatalf("129 overwrote an administrator's note: %q", n)
	}
}
