package calculation

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// EngineVersion numbers the arithmetic of this calculation engine. Bump it in
// the change that alters what a stored result would be — a rollup, a total, a
// function — so that every database recalculates once on its next start
// (RunEngineUpgrade) instead of showing its old rows until an input changes.
//
//	1  2026-10-05: plain references ignore pins of dimensions their source
//	   neither has nor relates to; formula totals evaluate at a scope's pins;
//	   a dimensionless rule-none metric keeps its value.
//	2  2026-10-07: a time summary of average counts every period of the
//	   reduction, an empty one as 0 (FY = the months' sum / 12).
const EngineVersion = 2

// engineUpgradeLease is how long a replica's claim on the sweep holds
// without being renewed; the claim is renewed after every revision.
const engineUpgradeLease = 10 * time.Minute

// RunEngineUpgrade recalculates every calculated metric of every revision in
// the database once, when runtime.calc_engine_state says its results come from
// an older EngineVersion, then records the new version. One replica runs it
// (a lease in the state row); another takes over a lapsed claim. Recalculation
// is idempotent, so a sweep cut short starts over on the next start.
func RunEngineUpgrade(ctx context.Context, pool *pgxpool.Pool, log zerolog.Logger) {
	host, _ := os.Hostname()
	owner := host + "-" + time.Now().UTC().Format(time.RFC3339Nano)
	claimed, err := claimEngineUpgrade(ctx, pool, owner)
	if err != nil {
		log.Warn().Err(err).Msg("calculation engine upgrade: claim")
		return
	}
	if !claimed {
		return
	}
	start := time.Now()
	revisions, err := revisionsWithCalculations(ctx, pool)
	if err != nil {
		log.Warn().Err(err).Msg("calculation engine upgrade: list revisions")
		return
	}
	log.Info().Int("revisions", len(revisions)).Int("engine_version", EngineVersion).
		Msg("calculation engine upgraded: recalculating every revision")
	sched := NewScheduler(log, NewStore(pool), nil)
	failed := 0
	for _, r := range revisions {
		if ctx.Err() != nil {
			return // shutting down: the next start sweeps again
		}
		if err := sched.RecalcSpecific(ctx, r.modelID, r.revisionID, r.metricIDs); err != nil {
			failed++
			log.Warn().Err(err).Str("model", r.modelID).Str("revision", r.revisionID).Msg("calculation engine upgrade: recalc")
		}
		_, _ = pool.Exec(ctx, `UPDATE runtime.calc_engine_state SET claimed_at = now() WHERE claimed_by = $1`, owner)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runtime.calc_engine_state SET version = $2, claimed_by = NULL, claimed_at = NULL, updated_at = now()
		WHERE claimed_by = $1`, owner, EngineVersion); err != nil {
		log.Warn().Err(err).Msg("calculation engine upgrade: record version")
		return
	}
	log.Info().Int("revisions", len(revisions)).Int("failed", failed).Dur("took", time.Since(start)).
		Msg("calculation engine upgrade: every revision recalculated")
}

// claimEngineUpgrade takes the sweep when the stored version is older and no
// live claim holds it.
func claimEngineUpgrade(ctx context.Context, pool *pgxpool.Pool, owner string) (bool, error) {
	var claimed bool
	err := pool.QueryRow(ctx, `
		UPDATE runtime.calc_engine_state SET claimed_by = $1, claimed_at = now()
		WHERE version < $2 AND (claimed_at IS NULL OR claimed_at < now() - $3::interval)
		RETURNING true`, owner, EngineVersion, engineUpgradeLease.String()).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return claimed, err
}

type revisionCalcs struct {
	modelID, revisionID string
	metricIDs           []string
}

func revisionsWithCalculations(ctx context.Context, pool *pgxpool.Pool) ([]revisionCalcs, error) {
	rows, err := pool.Query(ctx, `
		SELECT model_id::text, revision_id::text, array_agg(id::text ORDER BY name)
		FROM model.metric_def
		WHERE NOT is_input AND revision_id IS NOT NULL AND COALESCE(formula, '') <> ''
		GROUP BY model_id, revision_id
		ORDER BY model_id, revision_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []revisionCalcs
	for rows.Next() {
		var r revisionCalcs
		if err := rows.Scan(&r.modelID, &r.revisionID, &r.metricIDs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
