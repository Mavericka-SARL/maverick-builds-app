package fairshare

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func turn(t *testing.T, l *Limiter, key string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, err := l.Acquire(ctx, key)
	if err != nil {
		t.Fatalf("%s waited for a turn: %v", key, err)
	}
	return release
}

// waits reports whether key's Acquire is still waiting after a moment.
func waits(l *Limiter, key string) (bool, func()) {
	got := make(chan func(), 1)
	go func() {
		_, release, err := l.Acquire(context.Background(), key)
		if err == nil {
			got <- release
		}
	}()
	select {
	case r := <-got:
		return false, r
	case <-time.After(50 * time.Millisecond):
		return true, func() {
			if r := <-got; r != nil {
				r()
			}
		}
	}
}

// One tenant asking for more than its share waits behind its own work; a
// tenant arriving after it is served at once.
func TestATenantBeyondItsShareWaitsAndOthersDoNot(t *testing.T) {
	l := New(4, 2)
	a1, a2 := turn(t, l, "a"), turn(t, l, "a")
	stuck, a3 := waits(l, "a")
	if !stuck {
		t.Fatal("a third turn of tenant a was given past its share of 2")
	}
	b1 := turn(t, l, "b") // must not wait for a
	b2 := turn(t, l, "b")
	a1() // a's turn frees: its own waiting work takes it
	a3()
	a2()
	b1()
	b2()
	if l.inUse != 0 || len(l.running) != 0 || len(l.waiting) != 0 {
		t.Errorf("after every release: inUse %d running %v waiting %v", l.inUse, l.running, l.waiting)
	}
}

// When every turn is held, the next free one goes to the waiting tenant
// holding the fewest — c, holding none, before a's surplus.
func TestAFreedTurnGoesToTheTenantHoldingTheFewest(t *testing.T) {
	l := New(3, 2)
	a1, a2, b1 := turn(t, l, "a"), turn(t, l, "a"), turn(t, l, "b")
	order := make(chan string, 2)
	start := func(key string) {
		go func() {
			_, release, err := l.Acquire(context.Background(), key)
			if err == nil {
				order <- key
				time.Sleep(10 * time.Millisecond)
				release()
			}
		}()
	}
	start("b") // b holds 1, waits for a second: arrives first
	time.Sleep(20 * time.Millisecond)
	start("c") // c holds none
	time.Sleep(20 * time.Millisecond)
	a1() // one turn frees: c (0 held) before b (1 held)
	if got := <-order; got != "c" {
		t.Errorf("the freed turn went to %s, want c (holding none)", got)
	}
	a2()
	if got := <-order; got != "b" {
		t.Errorf("the next turn went to %s, want b", got)
	}
	b1()
}

// A caller that goes away while waiting leaves the queue and takes nothing.
func TestAWaitEndsWithItsCaller(t *testing.T) {
	l := New(1, 1)
	release := turn(t, l, "a")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := l.Acquire(ctx, "b"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	release()
	if l.inUse != 0 || len(l.waiting) != 0 {
		t.Errorf("after the cancelled wait: inUse %d waiting %v", l.inUse, l.waiting)
	}
	turn(t, l, "b")()
}

// Work already holding a turn goes on: a grid read answering a calculated
// member reads its siblings without waiting for turns of its own tenant.
func TestWorkHoldingATurnDoesNotWaitForAnother(t *testing.T) {
	l := New(1, 1)
	ctx, release, err := l.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, inner, err := l.Acquire(ctx, "a")
		inner()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("work holding a turn waited for another")
	}
	release()
	release() // a second release changes nothing
	if l.inUse != 0 {
		t.Errorf("inUse = %d after release, want 0", l.inUse)
	}
}

// Many tenants at once: never more than total turns, never more than a
// tenant's share, and every caller is served (run with -race).
func TestTurnsUnderLoad(t *testing.T) {
	l := New(4, 2)
	var inUse atomic.Int32
	perKey := map[string]*atomic.Int32{"a": {}, "b": {}, "c": {}}
	var wg sync.WaitGroup
	var served atomic.Int32
	for i := range 90 {
		key := []string{"a", "b", "c"}[i%3]
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := l.Acquire(context.Background(), key)
			if err != nil {
				t.Error(err)
				return
			}
			if n := inUse.Add(1); n > 4 {
				t.Errorf("%d turns at once, want at most 4", n)
			}
			if n := perKey[key].Add(1); n > 2 {
				t.Errorf("%s holds %d turns, want at most 2", key, n)
			}
			time.Sleep(time.Millisecond)
			perKey[key].Add(-1)
			inUse.Add(-1)
			served.Add(1)
			release()
		}()
	}
	wg.Wait()
	if served.Load() != 90 {
		t.Errorf("%d of 90 served", served.Load())
	}
}
