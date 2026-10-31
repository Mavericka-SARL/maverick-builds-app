package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A call that sends nothing for the limit is stopped and reported stalled; a
// call that keeps sending runs past the limit; a parent's cancellation is
// not a stall.
func TestWatchedAICallStopsASilentCall(t *testing.T) {
	limit := 40 * time.Millisecond
	stalled, err := watchedAICall(context.Background(), limit, func(ctx context.Context, _ func()) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !stalled || err == nil {
		t.Fatalf("silent call: stalled=%v err=%v, want stalled with an error", stalled, err)
	}

	stalled, err = watchedAICall(context.Background(), limit, func(ctx context.Context, touch func()) error {
		for i := 0; i < 8; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(limit / 4):
				touch()
			}
		}
		return nil
	})
	if stalled || err != nil {
		t.Fatalf("talking call: stalled=%v err=%v, want it to finish", stalled, err)
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	stalled, err = watchedAICall(parent, limit, func(ctx context.Context, _ func()) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if stalled || !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped turn: stalled=%v err=%v, want a cancellation, not a stall", stalled, err)
	}
}
