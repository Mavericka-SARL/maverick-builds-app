package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Every service treats a failed Connect as fatal, so one refused connection at
// boot ends the process. On a fresh rollout that is a race, not an error — the
// gateway lost it against pgbouncer on 2026-08-19 and exited with "connection
// refused", recovering only because Kubernetes restarted it.
func TestConnectRetriesUntilDeadlineThenReports(t *testing.T) {
	// Nothing is listening here, so every attempt fails the same way.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := Connect(ctx, "postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error connecting to a port nothing listens on")
	}
	// It must have retried rather than giving up on the first refusal, and it
	// must stop when the caller's context does rather than running to its own
	// 90s deadline.
	if elapsed < 400*time.Millisecond {
		t.Errorf("returned after %s — too fast to have retried at all", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %s — a cancelled context should end the wait immediately", elapsed)
	}
	if !strings.Contains(err.Error(), "ping database") {
		t.Errorf("error = %v, want it to name the failing step", err)
	}
}

// A URL that cannot be parsed is not a timing problem, and waiting 90 seconds
// to say so would turn a typo into a deploy that looks hung.
func TestConnectFailsFastOnAMalformedURL(t *testing.T) {
	start := time.Now()
	_, err := Connect(context.Background(), "://not a url")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s — a malformed URL must fail immediately, not retry", elapsed)
	}
	if !strings.Contains(err.Error(), "parse database url") {
		t.Errorf("error = %v, want a parse error", err)
	}
}
