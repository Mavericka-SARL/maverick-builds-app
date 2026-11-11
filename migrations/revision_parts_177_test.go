package migrations_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/migrations"
)

// TestRevisionPartsOutsideTransactions177 applies migrations 001–177. A
// transaction creating a revision takes no lock on models or metrics (176
// attached the revision's part from a trigger, and held their writes until
// that transaction ended); the revision's results go to the default part
// until runtime.add_revision_parts moves them into its own; that function
// skips a revision another transaction is still creating, and gives up
// after two seconds on a lock it cannot take; a revision's delete still
// empties its part, and its rows left in the default part go with it.
func TestRevisionPartsOutsideTransactions177(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("revparts177_%d", time.Now().UnixNano())
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
	other, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close(context.Background()) })

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") && e.Name() < "178" {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := fs.ReadFile(migrations.FS, f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		return n
	}
	add := func(rev string) (bool, time.Duration) {
		t.Helper()
		started := time.Now()
		var done bool
		if err := conn.QueryRow(ctx, `SELECT runtime.add_revision_parts($1::uuid)`, rev).Scan(&done); err != nil {
			t.Fatalf("add_revision_parts(%s): %v", rev, err)
		}
		return done, time.Since(started)
	}
	part := func(rev string) string { return "runtime.calc_result_" + strings.ReplaceAll(rev, "-", "") }
	inPart := func(rev string) int {
		t.Helper()
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, part(rev)).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			return -1
		}
		return count(`SELECT count(*) FROM ` + part(rev))
	}
	inDefault := func(rev string) int {
		t.Helper()
		return count(`SELECT count(*) FROM runtime.calc_result_default WHERE revision_id = $1::uuid`, rev)
	}

	customer, app, model := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO core.customer (id, name) VALUES ($1::uuid, 'Acme')`, customer)
	exec(`INSERT INTO core.application (id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Plan', 'planning')`, app, customer)
	exec(`INSERT INTO core.model (id, application_id, name) VALUES ($1::uuid, $2::uuid, 'Budget')`, model, app)
	newRevision := func(name string) (rev, metric string) {
		t.Helper()
		rev, metric = uuid.NewString(), uuid.NewString()
		exec(`INSERT INTO model.revision (id, model_id, name) VALUES ($1::uuid, $2::uuid, $3)`, rev, model, name)
		exec(`INSERT INTO model.metric_def (id, model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, $3::uuid, 'sales')`, metric, model, rev)
		return rev, metric
	}
	result := func(rev, metric, region string) {
		t.Helper()
		exec(`INSERT INTO runtime.calc_result (model_id, revision_id, metric_id, dim_members, value)
		      VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object('region', $4::text), 1)`, model, rev, metric, region)
	}

	// Creating a revision locks neither models nor metrics.
	tx, err := other.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pending := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO model.revision (id, model_id, name) VALUES ($1::uuid, $2::uuid, 'Pending')`, pending, model); err != nil {
		t.Fatal(err)
	}
	var held []string
	rows, err := tx.Query(ctx, `SELECT l.relation::regclass::text || ' ' || l.mode FROM pg_locks l
		WHERE l.pid = pg_backend_pid() AND l.relation IN ('core.model'::regclass, 'model.metric_def'::regclass, 'runtime.calc_result'::regclass)
		  AND l.mode NOT IN ('AccessShareLock', 'RowShareLock')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		held = append(held, s)
	}
	rows.Close()
	if len(held) > 0 {
		t.Errorf("a transaction creating a revision holds %v; want no lock on models, metrics or results", held)
	}
	// Its parts wait for its commit: no wait, no part.
	if done, took := add(pending); done || took > time.Second || inPart(pending) != -1 {
		t.Errorf("parts of an uncommitted revision: done %v after %v, part rows %d; want skipped at once", done, took, inPart(pending))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Results go to the default part until the revision's own is made; it
	// then holds them.
	revA, metA := newRevision("A")
	result(revA, metA, "north")
	result(revA, metA, "south")
	if inPart(revA) != -1 || inDefault(revA) != 2 {
		t.Fatalf("before its parts: own part %d, default %d; want none and 2", inPart(revA), inDefault(revA))
	}
	if n := count(`SELECT count(*) FROM runtime.revisions_without_parts() r WHERE r = $1::uuid`, revA); n != 1 {
		t.Errorf("revisions_without_parts lists A %d times, want once", n)
	}
	if done, _ := add(revA); !done || inPart(revA) != 2 || inDefault(revA) != 0 {
		t.Errorf("after its parts: done %v, own part %d, default %d; want true, 2, 0", done, inPart(revA), inDefault(revA))
	}
	if done, _ := add(revA); !done {
		t.Error("a second call for A is not done")
	}

	// A lock it cannot take within two seconds: it gives up, and the
	// revision is tried again later.
	revB, metB := newRevision("B")
	result(revB, metB, "north")
	hold, err := other.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `UPDATE model.metric_def SET name = name WHERE id = $1::uuid`, metA); err != nil {
		t.Fatal(err)
	}
	if done, took := add(revB); done || took < time.Second || took > 8*time.Second || inPart(revB) != -1 || inDefault(revB) != 1 {
		t.Errorf("under a held lock: done %v after %v, part %d, default %d; want false after about 2 s, rows still in default",
			done, took, inPart(revB), inDefault(revB))
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if done, _ := add(revB); !done || inPart(revB) != 1 {
		t.Errorf("once free: done %v, part %d; want true, 1", done, inPart(revB))
	}

	// The order a model's delete takes its locks — the model, then its
	// results in the default part — while the parts are being made: the
	// parts wait for the model first, so the delete's results go through
	// and nothing deadlocks.
	revD, metD := newRevision("D")
	result(revD, metD, "north")
	result(revD, metD, "south")
	del, err := other.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := del.Exec(ctx, `UPDATE core.model SET name = name WHERE id = $1::uuid`, model); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		done bool
		err  error
	}
	adding := make(chan outcome, 1)
	go func() {
		var done bool
		err := conn.QueryRow(ctx, `SELECT runtime.add_revision_parts($1::uuid)`, revD).Scan(&done)
		adding <- outcome{done, err}
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := del.Exec(ctx, `SET LOCAL lock_timeout = '1500ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err := del.Exec(ctx, `DELETE FROM runtime.calc_result WHERE revision_id = $1::uuid AND dim_members->>'region' = 'south'`, revD); err != nil {
		t.Errorf("a delete holding the model could not reach the default part while the parts waited: %v", err)
	}
	if err := del.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if o := <-adding; o.err != nil || !o.done {
		t.Errorf("parts made after the delete: done %v, err %v", o.done, o.err)
	}
	if inPart(revD) != 1 {
		t.Errorf("D's part holds %d results after the delete, want 1", inPart(revD))
	}

	// Deleting a revision empties its part; its rows in the default part go too.
	revC, metC := newRevision("C")
	result(revC, metC, "north")
	exec(`DELETE FROM model.revision WHERE id = $1::uuid`, revA)
	exec(`DELETE FROM model.revision WHERE id = $1::uuid`, revC)
	if n := count(`SELECT pg_relation_size($1::regclass)::int`, part(revA)); n != 0 {
		t.Errorf("deleted A's part holds %d bytes", n)
	}
	if inDefault(revC) != 0 {
		t.Errorf("deleted C keeps %d results in the default part", inDefault(revC))
	}
	if n := count(`SELECT count(*) FROM runtime.gone_revision_parts()`); n != 1 {
		t.Errorf("%d parts of gone revisions, want 1 (A's; never the default part)", n)
	}
}
