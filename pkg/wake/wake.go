// Package wake lets a background loop sleep while its tenant is quiet.
//
// With a database per tenant, every process runs each background loop once
// per tenant, and a loop that polled on a plain timer would query every
// tenant database every few seconds whether anything happened there or not.
// A Gate stands between the loop and its timer: while the tenant is active
// the loop keeps its usual interval, and while the tenant is quiet the loop
// waits until something wakes it — activity, or the time of the tenant's
// next scheduled item (pkg/tenantdb's Waker).
package wake

import (
	"context"
	"time"
)

// Gate decides when a background loop runs its next pass.
type Gate interface {
	// Wait returns true when the loop should run its next pass: after d
	// while the tenant is active, or as soon as it wakes while it is quiet.
	// It returns false once ctx is done.
	Wait(ctx context.Context, d time.Duration) bool
	// Touch reports that a pass found work, which keeps the tenant awake
	// and wakes the tenant's other loops, here and in other processes.
	Touch()
}

// Timer is the gate of a database that never sleeps (the shared database,
// or a single tenant): Wait is a plain sleep and Touch does nothing.
type Timer struct{}

// Wait sleeps d, or returns false when ctx ends first.
func (Timer) Wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Touch does nothing: there is nothing to keep awake.
func (Timer) Touch() {}

// Loop runs pass when g first lets it (immediately for an active tenant),
// then every d while the tenant is active, until ctx is done. A pass that
// reports work touches the gate.
func Loop(ctx context.Context, g Gate, d time.Duration, pass func(ctx context.Context) (didWork bool)) {
	wait := time.Duration(0)
	for g.Wait(ctx, wait) {
		if pass(ctx) {
			g.Touch()
		}
		wait = d
	}
}

// OrTimer returns g, or Timer when g is nil, so a loop built without a gate
// behaves as it always did.
func OrTimer(g Gate) Gate {
	if g == nil {
		return Timer{}
	}
	return g
}
