package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/calculation"
)

// TestBackgroundRecalcSurvivesPanic: a background recalculation that panics
// is logged and ends its goroutine, and never stops the gateway; its context
// carries the recalculation deadline.
func TestBackgroundRecalcSurvivesPanic(t *testing.T) {
	h := &handler{log: zerolog.Nop()}
	finished := make(chan time.Time, 1)
	go func() {
		defer func() { finished <- time.Now() }()
		ctx, done := h.backgroundRecalc(context.Background(), "test")
		defer done()
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > calculation.RecalcTimeout || time.Until(deadline) < calculation.RecalcTimeout-time.Minute {
			t.Errorf("deadline %v (set %v), want about %v from now", deadline, ok, calculation.RecalcTimeout)
		}
		panic("boom")
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the goroutine did not finish")
	}
}
