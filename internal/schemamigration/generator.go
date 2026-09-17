package schemamigration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type migrationFile struct {
	Filename    string `json:"filename"`
	SQL         string `json:"sql"`
	RollbackSQL string `json:"rollback_sql"`
	Checksum    string `json:"checksum"`
}

type Generator struct {
	pool *pgxpool.Pool
}

func NewGenerator(pool *pgxpool.Pool) *Generator { return &Generator{pool: pool} }

// Generate produces the SQL migration files for a model at a given version.
// It creates two convenience views over runtime.fact_input and runtime.calc_result
// that are scoped to this model, making BI tooling and ad-hoc queries simpler.
func (g *Generator) Generate(ctx context.Context, modelID string, versionNum int32) ([]migrationFile, error) {
	if err := g.assertModelExists(ctx, modelID); err != nil {
		return nil, err
	}

	sid := shortID(modelID)
	now := time.Now().UTC()

	inputView := fmt.Sprintf("runtime.m_%s_inputs", sid)
	resultView := fmt.Sprintf("runtime.m_%s_results", sid)
	filename := fmt.Sprintf("%04d_model_%s_views.sql", versionNum, sid)

	var sb strings.Builder
	fmt.Fprintf(&sb, "-- Model %s — schema migration v%d\n", modelID, versionNum)
	fmt.Fprintf(&sb, "-- Generated at %s\n\n", now.Format(time.RFC3339))

	// Input values view: latest aggregated input per metric + dimension slice
	fmt.Fprintf(&sb, "CREATE OR REPLACE VIEW %s AS\n", inputView)
	sb.WriteString("SELECT\n")
	sb.WriteString("    fi.revision_name,\n")
	sb.WriteString("    fi.dim_members,\n")
	sb.WriteString("    md.name         AS metric_name,\n")
	sb.WriteString("    md.id           AS metric_id,\n")
	sb.WriteString("    SUM(fi.value)   AS value,\n")
	sb.WriteString("    MAX(fi.entered_at) AS last_updated\n")
	sb.WriteString("FROM runtime.fact_input fi\n")
	sb.WriteString("JOIN model.metric_def   md ON md.id = fi.metric_id\n")
	fmt.Fprintf(&sb, "WHERE fi.model_id = '%s'::uuid\n", modelID)
	sb.WriteString("  AND md.is_input = true\n")
	sb.WriteString("GROUP BY fi.revision_name, fi.dim_members, md.name, md.id;\n\n")

	// Calculated results view: latest value per metric + dimension slice
	fmt.Fprintf(&sb, "CREATE OR REPLACE VIEW %s AS\n", resultView)
	sb.WriteString("SELECT\n")
	sb.WriteString("    latest.revision_name,\n")
	sb.WriteString("    latest.dim_members,\n")
	sb.WriteString("    md.name  AS metric_name,\n")
	sb.WriteString("    md.id    AS metric_id,\n")
	sb.WriteString("    latest.value,\n")
	sb.WriteString("    latest.calc_at\n")
	sb.WriteString("FROM (\n")
	sb.WriteString("    SELECT DISTINCT ON (revision_name, metric_id, dim_members)\n")
	sb.WriteString("           revision_name, metric_id, dim_members, value, calc_at\n")
	sb.WriteString("    FROM runtime.calc_result\n")
	fmt.Fprintf(&sb, "    WHERE model_id = '%s'::uuid\n", modelID)
	sb.WriteString("    ORDER BY revision_name, metric_id, dim_members, calc_at DESC\n")
	sb.WriteString(") latest\n")
	sb.WriteString("JOIN model.metric_def md ON md.id = latest.metric_id;\n")

	forwardSQL := sb.String()

	rollbackSQL := fmt.Sprintf(
		"DROP VIEW IF EXISTS %s;\nDROP VIEW IF EXISTS %s;\n",
		resultView, inputView,
	)

	sum := sha256.Sum256([]byte(forwardSQL))

	return []migrationFile{{
		Filename:    filename,
		SQL:         forwardSQL,
		RollbackSQL: rollbackSQL,
		Checksum:    fmt.Sprintf("%x", sum),
	}}, nil
}

func (g *Generator) assertModelExists(ctx context.Context, modelID string) error {
	var exists bool
	err := g.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM model.metric_def WHERE model_id = $1 LIMIT 1)
	`, modelID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check model: %w", err)
	}
	if !exists {
		return fmt.Errorf("model %s has no metrics defined", modelID)
	}
	return nil
}

// shortID returns the first 8 hex characters of a UUID (dashes stripped),
// suitable for embedding in SQL identifiers.
func shortID(uuid string) string {
	clean := strings.ReplaceAll(uuid, "-", "")
	if len(clean) > 8 {
		return clean[:8]
	}
	return clean
}
