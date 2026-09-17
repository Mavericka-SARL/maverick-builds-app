package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

// retryTransient exists because applyFormMappings and recomputeFactInput run
// detached from the request: a failure there has no caller to return to and
// nothing re-runs it, so a single transient database error left the mapping's
// aggregate permanently wrong rather than merely late.
func TestRetryTransient(t *testing.T) {
	ctx := context.Background()

	t.Run("succeeds on a later attempt", func(t *testing.T) {
		calls := 0
		err := retryTransient(ctx, 3, func() error {
			calls++
			if calls < 3 {
				return errors.New("transient")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("want nil after eventual success, got %v", err)
		}
		if calls != 3 {
			t.Fatalf("want 3 attempts, got %d", calls)
		}
	})

	t.Run("does not retry a call that succeeds", func(t *testing.T) {
		calls := 0
		if err := retryTransient(ctx, 3, func() error { calls++; return nil }); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if calls != 1 {
			t.Fatalf("a successful op must run exactly once, ran %d times", calls)
		}
	})

	t.Run("gives up and returns the last error", func(t *testing.T) {
		sentinel := errors.New("still broken")
		calls := 0
		err := retryTransient(ctx, 3, func() error { calls++; return sentinel })
		if !errors.Is(err, sentinel) {
			t.Fatalf("want the operation's own error back, got %v", err)
		}
		if calls != 3 {
			t.Fatalf("want 3 attempts before giving up, got %d", calls)
		}
	})

	t.Run("stops when the context is done", func(t *testing.T) {
		// The backoff must not outlive a cancelled context, or shutdown waits
		// on retries of work that can no longer succeed.
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		start := time.Now()
		err := retryTransient(cctx, 5, func() error { calls++; return errors.New("nope") })
		if err == nil {
			t.Fatal("want an error from a cancelled context")
		}
		if calls != 1 {
			t.Fatalf("want the first attempt only, got %d", calls)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("cancelled retry should return promptly, took %s", elapsed)
		}
	})
}
