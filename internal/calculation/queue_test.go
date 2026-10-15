package calculation

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// waitFor polls cond until it holds or a second has passed.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Requests that arrive while a pass runs share the next pass: forty
// uploads used to queue forty passes, each recalculating what the next one
// would replace.
func TestPassQueueFoldsWaitingRequestsIntoOnePass(t *testing.T) {
	q := queueFor(nil, "m-fold", "r-fold")
	var mu sync.Mutex
	var passes [][]string
	release := make(chan struct{})
	pass := func(ids []string) error {
		mu.Lock()
		passes = append(passes, ids)
		first := len(passes) == 1
		mu.Unlock()
		if first {
			<-release // the first pass runs while the others arrive
		}
		return nil
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- q.run([]string{"a"}, pass) }()
	waitFor(t, "the first pass to start", func() bool { mu.Lock(); defer mu.Unlock(); return len(passes) == 1 })

	var wg sync.WaitGroup
	for _, id := range []string{"b", "c", "d", "e"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := q.run([]string{id}, pass); err != nil {
				t.Errorf("run %s: %v", id, err)
			}
		}()
	}
	waitFor(t, "the requests to wait", func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.pending) == 4 })
	close(release)
	wg.Wait()
	if err := <-firstDone; err != nil {
		t.Fatalf("first run: %v", err)
	}

	if len(passes) != 2 {
		t.Fatalf("%d passes ran (%v), want 2: the first, and one for every request that waited", len(passes), passes)
	}
	if !slices.Equal(passes[1], []string{"b", "c", "d", "e"}) {
		t.Errorf("the second pass calculated %v, want b, c, d and e", passes[1])
	}
}

// A request that a pass it did not run served gets that pass's error.
func TestPassQueueGivesWaitersTheirPassError(t *testing.T) {
	q := queueFor(nil, "m-err", "r-err")
	release := make(chan struct{})
	started := make(chan struct{})
	boom := errors.New("dependency order: a cycle")
	calls := 0
	pass := func(ids []string) error {
		calls++
		if calls == 1 {
			close(started)
			<-release
			return nil
		}
		return boom
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- q.run([]string{"a"}, pass) }()
	<-started
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- q.run([]string{"b"}, pass) }()
	}
	waitFor(t, "the requests to wait", func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.pending) == 1 })
	close(release)
	if err := <-firstDone; err != nil {
		t.Errorf("the first pass succeeded, its caller got %v", err)
	}
	for range 2 {
		if err := <-errs; !errors.Is(err, boom) {
			t.Errorf("a waiter of the failed pass got %v, want its error", err)
		}
	}
}

// A pass that panics is recorded finished: the revision's next request
// still runs instead of waiting on a pass that is gone.
func TestPassQueueSurvivesAPanickingPass(t *testing.T) {
	q := queueFor(nil, "m-panic", "r-panic")
	func() {
		defer func() { _ = recover() }()
		_ = q.run([]string{"a"}, func([]string) error { panic("evaluator bug") })
	}()
	done := make(chan error, 1)
	go func() { done <- q.run([]string{"b"}, func([]string) error { return nil }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the request after a panic: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the request after a panicking pass never ran")
	}
}
