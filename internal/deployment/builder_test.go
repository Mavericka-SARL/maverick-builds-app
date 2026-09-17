// Tests Builder.Build against a real, fully migrated Postgres schema — the
// previous implementation had zero coverage of Build/loadSchema at all
// (only Store's job-row CRUD was tested, against a stale hand-maintained
// fixture that fabricated the very `dd.code` column bug this batch fixes).
package deployment_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mavericks-engine/mavericks/internal/deployment"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

func startPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("mavericks"),
		tcpostgres.WithUsername("mavericks"),
		tcpostgres.WithPassword("mavericks"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pgc.Terminate(ctx) })

	dsn, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// setupBuilderDB migrates a real, fully-migrated schema via migrationfs.FS
// (the same mechanism cmd/gateway and every gateway integration test uses)
// — replacing the previous stale, hand-maintained testdata/001_schema.sql
// fixture that had drifted from migrations/ enough to mask the dd.code bug.
func setupBuilderDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := startPostgres(t)
	if err := migrate.Run(context.Background(), pool, migrationfs.FS, "."); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// seedFullEntityGraph seeds a model with representatives of several entity
// kinds (dimensions+members, metrics, grids, forms, facts) — enough to
// prove Build pulls a real multi-kind package via modeltransfer.CollectExport,
// not just the old implementation's dimensions+metrics.
func seedFullEntityGraph(t *testing.T, pool *pgxpool.Pool) (modelID, revisionID string) {
	t.Helper()
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('BuilderCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, appID)
	revisionID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revisionID, modelID)
	userID := q(`INSERT INTO identity.user (keycloak_sub, email) VALUES ('builder-test', 'builder@test.dev') RETURNING id::text`)

	dimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, agg_rule) VALUES ($1::uuid, $2::uuid, 'Department', 'sum') RETURNING id::text`, modelID, revisionID)
	exec(`INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid, 'ENG', 'Engineering', 0)`, dimID)
	metricID := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'revenue', true, 'sum') RETURNING id::text`, modelID, revisionID)
	gridID := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Grid') RETURNING id::text`, modelID, revisionID)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 0)`, gridID, metricID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, gridID, dimID)
	exec(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid, $2::uuid, 'f', 'F', '[]'::jsonb)`, modelID, revisionID)
	exec(`
		INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by)
		VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, jsonb_build_object($4::text,'ENG'), 100, $5::uuid)`,
		modelID, revisionID, metricID, dimID, userID)

	return modelID, revisionID
}

// extractTarGz reads an in-memory tar.gz archive and returns its entries
// by name.
func extractTarGz(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()

	gr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close() //nolint:errcheck

	tr := tar.NewReader(gr)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil { //nolint:gosec
			t.Fatalf("tar copy %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = buf.Bytes()
	}
	return out
}

func TestBuild_ProducesFullEntityGraphAndRealMigrations(t *testing.T) {
	pool := setupBuilderDB(t)
	modelID, revisionID := seedFullEntityGraph(t, pool)

	b := deployment.NewBuilder(pool, logger.New("test"))
	archive, resolvedRevID, err := b.Build(context.Background(), modelID, revisionID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resolvedRevID != revisionID {
		t.Errorf("resolved revision id = %q, want %q", resolvedRevID, revisionID)
	}

	files := extractTarGz(t, archive)

	pkgRaw, ok := files["mavericks-model/package.json"]
	if !ok {
		t.Fatal("archive missing mavericks-model/package.json")
	}
	var pkg modeltransfer.Package
	if err := json.Unmarshal(pkgRaw, &pkg); err != nil {
		t.Fatalf("unmarshal package.json: %v", err)
	}
	if len(pkg.Dimensions) != 1 || len(pkg.Dimensions[0].Members) != 1 {
		t.Errorf("dimensions = %+v, want 1 dimension with 1 member", pkg.Dimensions)
	}
	if len(pkg.Metrics) != 1 {
		t.Errorf("metrics = %d, want 1", len(pkg.Metrics))
	}
	// The real, previously-uncovered bug: the old loadSchema queried a
	// column (dd.code) that has never existed on model.dimension_def and
	// silently discarded the resulting error, and never gathered grids,
	// forms, or facts at all. Asserting these are non-empty proves Build
	// now goes through modeltransfer.CollectExport for real, against the
	// real production schema, not the old broken/narrow implementation.
	if len(pkg.Grids) != 1 || len(pkg.Grids[0].Metrics) != 1 || len(pkg.Grids[0].Dimensions) != 1 {
		t.Errorf("grids = %+v, want 1 grid with 1 metric + 1 dimension", pkg.Grids)
	}
	if len(pkg.Forms) != 1 {
		t.Errorf("forms = %d, want 1", len(pkg.Forms))
	}
	if len(pkg.Facts) != 1 {
		t.Errorf("facts = %d, want 1", len(pkg.Facts))
	}
	if pkg.FactsPolicy != modeltransfer.FactsPolicy {
		t.Errorf("facts_policy = %q, want %q", pkg.FactsPolicy, modeltransfer.FactsPolicy)
	}

	manifestRaw, ok := files["mavericks-model/manifest.json"]
	if !ok {
		t.Fatal("archive missing mavericks-model/manifest.json")
	}
	var manifest struct {
		KnownGaps []string `json:"known_gaps"`
		Runtime   struct {
			BuildCommand string `json:"build_command"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	if len(manifest.KnownGaps) == 0 {
		t.Error("manifest.known_gaps is empty, want the access-config/tests gaps documented explicitly")
	}
	if manifest.Runtime.BuildCommand == "" {
		t.Error("manifest.runtime.build_command is empty")
	}

	if _, ok := files["mavericks-model/docker-compose.yml"]; !ok {
		t.Error("archive missing docker-compose.yml")
	}
	if _, ok := files["mavericks-model/README.md"]; !ok {
		t.Error("archive missing README.md")
	}

	// migrations/ must contain the REAL, full migration tree — the only
	// thing that lets this package bootstrap a genuinely empty database.
	realMigrations, err := migrationfs.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read real migrations dir: %v", err)
	}
	wantCount := 0
	for _, e := range realMigrations {
		if !e.IsDir() {
			wantCount++
		}
	}
	gotCount := 0
	for name := range files {
		if len(name) > len("mavericks-model/migrations/") && name[:len("mavericks-model/migrations/")] == "mavericks-model/migrations/" {
			gotCount++
		}
	}
	if gotCount != wantCount {
		t.Errorf("packaged migrations count = %d, want %d (the real migrations/ tree)", gotCount, wantCount)
	}
}

// TestBuild_SchemaMismatchFailsLoudly is a regression test for the exact
// bug this batch fixes: the old loadSchema discarded every query error
// (`if err == nil { ... }`), so a schema mismatch — e.g. the dd.code
// column that has never existed — produced an empty-but-"successful"
// archive instead of a build failure. Build now delegates to
// modeltransfer.CollectExport, which propagates every query error; this
// test proves it by pointing Build at a pool with NO schema applied at
// all (every query must fail).
func TestBuild_SchemaMismatchFailsLoudly(t *testing.T) {
	pool := startPostgres(t) // deliberately NOT migrated

	b := deployment.NewBuilder(pool, logger.New("test"))
	_, _, err := b.Build(context.Background(), "00000000-0000-0000-0000-000000000000", "")
	if err == nil {
		t.Fatal("expected Build to fail loudly against an unmigrated schema, got nil error")
	}
}
