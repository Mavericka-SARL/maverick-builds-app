package gateway

import (
	"context"

	"github.com/mavericks-engine/mavericks/internal/calculation"
)

// A write that answers before its dependents are recalculated leaves the
// revision waiting for a pass; runtime.revision_recalc_state counts what was
// asked for and what has run, and the grid read reports the gap
// (recalc_pending) so the console shows calculated cells as pending until
// the pass lands. A typed number on a 128-metric model used to wait ~4 s for
// the recalculation; a business user's removed row stayed in the totals for a
// minute with nothing saying so.

// recalcStaleAfter: a gap older than this is a pass cut short (a restart),
// not one still running.
const recalcStaleAfter = "15 minutes"

// recalcInBackground marks revisionID as awaiting a recalculation, then runs
// fn after the request has answered; the mark is lifted when fn ends, failed
// or not (a failed metric records its own error).
func (h *handler) recalcInBackground(ctx context.Context, revisionID, what string, fn func(ctx context.Context)) {
	seq := h.requestRecalc(ctx, revisionID)
	keep := context.WithoutCancel(ctx)
	go func() {
		bg, done := h.backgroundRecalc(keep, what)
		defer done()
		defer h.recalcDone(keep, revisionID, seq)
		fn(bg)
	}()
}

// recalcAfterImport recalculates what an import's metrics reach after the
// request has answered, as a cell write's recalculation runs: the grid shows
// the revision's calculated cells as pending until it lands. An import used
// to start a bare pass nothing recorded, so the grid showed old totals with
// nothing saying they were about to change.
func (h *handler) recalcAfterImport(ctx context.Context, modelID, revisionID, what string, metricIDs []string) {
	sched := calculation.NewScheduler(h.log, calculation.NewStore(h.db.For(ctx)))
	h.recalcInBackground(ctx, revisionID, "recalculation after an "+what, func(bg context.Context) {
		_ = sched.RecalcAffected(bg, modelID, revisionID, metricIDs)
	})
}

func (h *handler) requestRecalc(ctx context.Context, revisionID string) int64 {
	var seq int64
	if err := h.db.For(ctx).QueryRow(ctx, `
		INSERT INTO runtime.revision_recalc_state (revision_id, requested, updated_at) VALUES ($1::uuid, 1, now())
		ON CONFLICT (revision_id) DO UPDATE SET requested = runtime.revision_recalc_state.requested + 1, updated_at = now()
		RETURNING requested`, revisionID).Scan(&seq); err != nil {
		h.log.Warn().Err(err).Str("revision", revisionID).Msg("record a pending recalculation")
	}
	return seq
}

func (h *handler) recalcDone(ctx context.Context, revisionID string, seq int64) {
	if _, err := h.db.For(ctx).Exec(ctx, `
		UPDATE runtime.revision_recalc_state SET done = GREATEST(done, $2), updated_at = now()
		WHERE revision_id = $1::uuid`, revisionID, seq); err != nil {
		h.log.Warn().Err(err).Str("revision", revisionID).Msg("record a finished recalculation")
	}
}

// recalcPending reports whether revisionID has a recalculation still to land.
func (h *handler) recalcPending(ctx context.Context, revisionID string) bool {
	var pending bool
	_ = h.db.For(ctx).QueryRow(ctx, `
		SELECT requested > done AND updated_at > now() - $2::interval
		FROM runtime.revision_recalc_state WHERE revision_id = $1::uuid`, revisionID, recalcStaleAfter).Scan(&pending)
	return pending
}
