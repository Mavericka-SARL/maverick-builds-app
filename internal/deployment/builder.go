// Package deployment builds a standalone deployment package for one
// model+revision: the model's full entity graph (via internal/modeltransfer
// — the same code the tenant_admin HTTP export endpoint uses), the complete
// database schema needed to bootstrap an empty Postgres, a provenance
// manifest, and an infra-only docker-compose. See Builder's doc comment for
// what this repairs relative to the previous implementation.
package deployment

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// Builder assembles a standalone deployment package for one model+revision,
// fully in memory — the caller (internal/gateway/model_export_package.go)
// is responsible for retention via pkg/objectstore; Builder itself never
// touches a filesystem.
//
// Repairs a previously broken implementation: the old loadSchema queried a
// `dd.code` column that has never existed on model.dimension_def in any
// migration, and silently discarded the resulting query error (`if err ==
// nil { ... }`) — so a schema mismatch produced an empty-but-"successful"
// archive instead of failing loudly. It also aggregated every revision of
// every model under an application in one pass, with no revision-scoping
// at all, and covered only 2 of the model's ~13 entity kinds (dimensions,
// metrics — no facts, grids, forms, dashboards, workflows). Build now
// delegates entity-graph collection to modeltransfer.CollectExport, the
// same code the tenant_admin HTTP export endpoint
// (internal/gateway/model_transfer.go) uses, re-scoped to one model+
// revision to match that contract — so both paths share one correct,
// tested implementation instead of two independent ones.
//
// It also used to write its tar.gz to local disk (os.TempDir() by
// default) — the only production code path anywhere in this repository
// that touched a filesystem, and genuinely broken under the real k8s
// topology (readOnlyRootFilesystem: true, no writable volume). Build now
// returns the archive bytes directly; nothing here writes to disk.
type Builder struct {
	pool *pgxpool.Pool
	log  zerolog.Logger
}

func NewBuilder(pool *pgxpool.Pool, log zerolog.Logger) *Builder {
	return &Builder{pool: pool, log: log}
}

// manifest records the package's provenance and known, deliberate gaps —
// surfaced as an explicit, inspectable file rather than left implicit or
// silently omitted.
type manifest struct {
	GeneratedAt time.Time  `json:"generated_at"`
	ModelID     string     `json:"model_id"`
	RevisionID  string     `json:"revision_id"`
	FactsPolicy string     `json:"facts_policy"`
	Runtime     runtimeDep `json:"runtime"`
	// KnownGaps documents what this package deliberately does NOT include
	// and why, rather than silently omitting them with no trace.
	KnownGaps []string `json:"known_gaps"`
}

// runtimeDep declares (rather than builds) what's needed to run this
// package's data against a live gateway — no Dockerfile exists anywhere in
// this repository, and building/publishing the real ~14-service image set
// is owned by a separate backlog item ("Distributed topology validation").
type runtimeDep struct {
	Repo         string `json:"repo"`
	CommitSHA    string `json:"commit_sha"`
	GoVersion    string `json:"go_version"`
	BuildCommand string `json:"build_command"`
}

var knownGaps = []string{
	"access configuration (security.* policy rows, identity.business_role* / user access rules) is not included: those rows are per-user or workspace-scoped and do not transfer meaningfully across the tenant boundary this package is designed to cross (source and destination users are never the same identities) — reconciling them against the importing tenant's own users is a separate, unbuilt feature.",
	"per-model test definitions are not included: no such entity exists anywhere in this schema to package (cmd/qa-engine-test is a standalone smoke-test CLI, not a per-model artifact).",
}

// Build gathers modelID's entity graph at revisionID (empty = the model's
// active revision, resolved the same way as the HTTP export endpoint), the
// complete migrations/ tree, a provenance manifest, and an infra-only
// docker-compose + README — packs them into a .tar.gz, and returns the
// archive bytes plus the resolved revision ID (the caller needs it for
// retention: pkg/objectstore keys/owns records by it).
func (b *Builder) Build(ctx context.Context, modelID, revisionID string) (data []byte, resolvedRevisionID string, err error) {
	return b.BuildWithOptions(ctx, modelID, revisionID, modeltransfer.ExportOptions{IncludeData: true})
}

// BuildWithOptions is Build with an explicit choice of whether the package
// carries the revision's data (fact values, form records) or its
// definitions only.
func (b *Builder) BuildWithOptions(ctx context.Context, modelID, revisionID string, opts modeltransfer.ExportOptions) (data []byte, resolvedRevisionID string, err error) {
	resolvedRevID, revisionName, err := modeltransfer.ResolveRevision(ctx, b.pool, modelID, revisionID)
	if err != nil {
		return nil, "", fmt.Errorf("resolve revision: %w", err)
	}
	pkg, err := modeltransfer.CollectExportWithOptions(ctx, b.pool, modelID, resolvedRevID, revisionName, opts)
	if err != nil {
		return nil, "", fmt.Errorf("collect export: %w", err)
	}

	files, err := b.renderFiles(pkg, modelID, resolvedRevID)
	if err != nil {
		return nil, "", fmt.Errorf("render files: %w", err)
	}

	archive, err := buildTarGz(files)
	if err != nil {
		return nil, "", fmt.Errorf("build archive: %w", err)
	}

	b.log.Info().
		Str("model_id", modelID).
		Str("revision_id", resolvedRevID).
		Int("files", len(files)).
		Int("archive_bytes", len(archive)).
		Msg("export package built")

	return archive, resolvedRevID, nil
}

// renderFiles produces the in-memory file set for the export archive.
func (b *Builder) renderFiles(pkg *modeltransfer.Package, modelID, revisionID string) (map[string][]byte, error) {
	const root = "mavericks-model"
	files := make(map[string][]byte)

	pkgJSON, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal package: %w", err)
	}
	files[root+"/package.json"] = pkgJSON

	m := manifest{
		GeneratedAt: time.Now().UTC(),
		ModelID:     modelID,
		RevisionID:  revisionID,
		FactsPolicy: modeltransfer.FactsPolicy,
		Runtime:     buildRuntimeDep(),
		KnownGaps:   knownGaps,
	}
	manifestJSON, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	files[root+"/manifest.json"] = manifestJSON

	if err := copyMigrations(files, root+"/migrations"); err != nil {
		return nil, fmt.Errorf("copy migrations: %w", err)
	}

	files[root+"/docker-compose.yml"] = renderDockerCompose()
	files[root+"/README.md"] = renderReadme(pkg, &m)

	return files, nil
}

// buildRuntimeDep reads the source commit this binary was built from (via
// Go's automatic VCS stamping — no shelling out to git, no dependency on a
// .git directory being present at runtime) rather than building or
// publishing an image.
func buildRuntimeDep() runtimeDep {
	dep := runtimeDep{
		Repo:         "https://github.com/mavericks-engine/mavericks",
		GoVersion:    goruntime.Version(),
		BuildCommand: "go build ./cmd/gateway && ./gateway  # self-migrates on boot (migrate.Run against migrations/)",
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				dep.CommitSHA = s.Value
			}
		}
	}
	return dep
}

// copyMigrations embeds the complete migrations/*.sql tree verbatim —
// there is no coherent "this model's migrations" separate from the whole
// schema, and this is what lets the package bootstrap a genuinely empty
// database on its own.
func copyMigrations(files map[string][]byte, prefix string) error {
	entries, err := fs.ReadDir(migrationfs.FS, ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		content, err := fs.ReadFile(migrationfs.FS, e.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		files[prefix+"/"+e.Name()] = content
	}
	return nil
}

func renderDockerCompose() []byte {
	return []byte(`version: "3.9"

# Infra only. This package ships DATA (package.json) and DDL (migrations/),
# not a container image — no Dockerfile exists in this repository yet
# (publishing real service images is a separate, not-yet-started backlog
# item). Bring this up, then build and run the gateway from source — see
# manifest.json's runtime.build_command — against it.

services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: mavericks
      POSTGRES_PASSWORD: mavericks
      POSTGRES_DB: mavericks
    ports:
      - "5432:5432"
    volumes:
      - postgres_data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U mavericks"]
      interval: 5s
      timeout: 3s
      retries: 10

volumes:
  postgres_data:
`)
}

func renderReadme(pkg *modeltransfer.Package, m *manifest) []byte {
	var gaps strings.Builder
	for _, g := range m.KnownGaps {
		fmt.Fprintf(&gaps, "- %s\n", g)
	}
	return []byte(fmt.Sprintf(`# %s (%s) — Mavericks Standalone Package

Generated: %s

## Contents

- package.json        — the model's full entity graph: definitions, facts, workflows, forms (see "Facts policy" below)
- migrations/          — the complete database schema (every migration this repo ships, applied in order)
- manifest.json        — provenance (source commit) and known limitations
- docker-compose.yml   — infra only (Postgres); no application image exists yet — build from source (see below)

## Facts policy

%s

## Known limitations

%s
## Running standalone

1. docker compose up -d
2. Clone %s at commit %s
3. %s
4. Import package.json into the running gateway: POST /api/admin/models/import (tenant_admin)
`,
		pkg.ModelName, pkg.RevisionName, m.GeneratedAt.Format(time.RFC3339),
		m.FactsPolicy, gaps.String(),
		m.Runtime.Repo, m.Runtime.CommitSHA, m.Runtime.BuildCommand,
	))
}

func buildTarGz(files map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(content)),
			ModTime: time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(content); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
