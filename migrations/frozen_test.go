package migrations

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// An applied migration is frozen: pkg/migrate matches what it has already
// run by filename AND checksum, so a database that ran a file refuses to
// start once that file changes — "migration NNN_x.sql checksum mismatch",
// on every existing deployment at once, at gateway start-up. Renaming one
// is the mirror image: the runner sees an unapplied migration and runs it
// again against a schema that already has it.
//
// Neither is caught by any other check here. Both nearly shipped on
// 2026-09-22 (editing 043's COMMENT to document a new widget type), which
// only failed CI because the generated sqlc models happened to move too.
//
// checksums.txt is therefore the record of what has been released. Adding a
// migration appends a line; changing or renaming an existing one changes a
// line, which is the reviewable act this test exists to force.
//
//	UPDATE_MIGRATION_LOCK=1 go test ./migrations/
//
// updates the file. Run it for a NEW migration. Reach for it on an existing
// one only when that migration has demonstrably never been applied anywhere
// — on any deployment, including staging and a colleague's dev database.
const lockFile = "checksums.txt"

var migrationName = regexp.MustCompile(`^\d{3}_[a-z0-9_]+\.sql$`)

func checksums(t *testing.T) ([]string, map[string]string) {
	t.Helper()
	entries, err := fs.Glob(FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(entries)
	out := map[string]string{}
	for _, name := range entries {
		body, err := FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	return entries, out
}

func TestMigrationsAreFrozen(t *testing.T) {
	names, sums := checksums(t)
	if len(names) == 0 {
		t.Fatal("no migrations found")
	}

	var lines []string
	for _, name := range names {
		lines = append(lines, sums[name]+"  "+name)
	}
	want := strings.Join(lines, "\n") + "\n"

	if os.Getenv("UPDATE_MIGRATION_LOCK") != "" {
		if err := os.WriteFile(lockFile, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d migrations)", lockFile, len(names))
		return
	}

	raw, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("%s is missing; create it with UPDATE_MIGRATION_LOCK=1 go test ./migrations/: %v", lockFile, err)
	}
	if string(raw) == want {
		return
	}

	// Say exactly what moved, because the three cases need different fixes.
	locked := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if sum, name, ok := strings.Cut(line, "  "); ok {
			locked[name] = sum
		}
	}
	var changed, removed, added []string
	for name, sum := range sums {
		switch prev, known := locked[name]; {
		case !known:
			added = append(added, name)
		case prev != sum:
			changed = append(changed, name)
		}
	}
	for name := range locked {
		if _, still := sums[name]; !still {
			removed = append(removed, name)
		}
	}
	sort.Strings(changed)
	sort.Strings(removed)
	sort.Strings(added)

	if len(changed) > 0 {
		t.Errorf("these migrations were edited after being released: %s\n"+
			"    Every database that already ran one refuses to start until it matches again.\n"+
			"    Restore the file and put the change in a NEW migration instead.", strings.Join(changed, ", "))
	}
	if len(removed) > 0 {
		t.Errorf("these migrations were deleted or renamed: %s\n"+
			"    A database that ran the old name keeps the record under that name; a new name is applied again.\n"+
			"    Restore the file under its original name.", strings.Join(removed, ", "))
	}
	if len(changed) == 0 && len(removed) == 0 && len(added) > 0 {
		t.Errorf("new migrations are not recorded yet: %s\n"+
			"    Run: UPDATE_MIGRATION_LOCK=1 go test ./migrations/", strings.Join(added, ", "))
	}
}

// Names already released that break the convention below. They cannot be
// corrected: a rename is applied again on every database that ran the old
// name (see TestMigrationsAreFrozen), which is a far worse trade than an
// ugly filename. New migrations get no such indulgence.
var legacyNames = map[string]string{
	"017_dimension_properties.sql":          "shares 017 with 017_crud_forms.sql; lexical order settles it, deterministically",
	"018_dimension_hierarchy_revisions.sql": "shares 018 with 018_automation.sql; likewise",
	"094.sql":                               "lost its words to a mv into /tmp and back while proving a test failed (2026-09-21); applied under this name",
}

// Migrations run in lexical filename order, so the names have to sort the
// way the numbers do — and two files may not claim the same number, or
// which of them runs first depends on the rest of the name.
func TestMigrationNamesAreOrderly(t *testing.T) {
	names, _ := checksums(t)
	seen := map[string]string{}
	for _, name := range names {
		if _, old := legacyNames[name]; old {
			continue
		}
		if !migrationName.MatchString(name) {
			t.Errorf("%s: name a migration NNN_lower_snake_case.sql — the number orders it, the words say what it does", name)
			continue
		}
		num := name[:3]
		if prev, dup := seen[num]; dup {
			t.Errorf("%s and %s share the number %s: which runs first depends on the rest of the name", prev, name, num)
		}
		seen[num] = name
	}
	// The indulgences must stay honest: one for a file that is gone is a
	// leftover, and every entry has to name a migration that exists.
	all, _ := checksums(t)
	have := map[string]bool{}
	for _, n := range all {
		have[n] = true
	}
	for name := range legacyNames {
		if !have[name] {
			t.Errorf("%s is excused in legacyNames but no longer exists — drop the entry", name)
		}
	}
}
