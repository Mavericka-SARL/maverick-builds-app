// Package fairshare keeps one tenant's heavy work from holding back
// another's.
//
// A gateway serves every tenant from the same processors. Before this
// package, one tenant opening dashboards over models with hundreds of
// calculated metrics ran dozens of chart and grid reads at once, Go shared
// the processors between goroutines, not tenants, and every other tenant's
// requests — a dashboard, the Users list — waited behind them (the
// Playground, 2026-11-18).
//
// Heavy work — a grid or chart read, one component of a recalculation pass —
// takes a turn from a Limiter under its tenant's key. A gateway runs at most
// total turns at once, and one tenant holds at most perKey of them, so the
// others always find turns free: a tenant's surplus waits behind its own
// work, never behind another tenant's. When a turn frees, it goes to the
// waiting tenant holding the fewest, the longest waiting among equals. Light
// requests take no turn at all.
package fairshare

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"sync"
)

// Limiter hands out turns for heavy work, fairly between keys (tenants).
type Limiter struct {
	total, perKey int

	mu      sync.Mutex
	inUse   int
	running map[string]int
	waiting map[string][]*waiter // per key, in arrival order
	seq     uint64
}

type waiter struct {
	ready   chan struct{}
	seq     uint64
	granted bool
}

// New is a Limiter running at most total turns at once, at most perKey of
// them for one key (both at least 1; perKey at most total).
func New(total, perKey int) *Limiter {
	if total < 1 {
		total = 1
	}
	if perKey < 1 {
		perKey = 1
	}
	if perKey > total {
		perKey = total
	}
	return &Limiter{total: total, perKey: perKey, running: map[string]int{}, waiting: map[string][]*waiter{}}
}

// Limits are the Limiter's total and per-key turns.
func (l *Limiter) Limits() (total, perKey int) { return l.total, l.perKey }

type heldKey struct{}

// Acquire waits for a turn for key and returns ctx marked as holding it, and
// the turn's release (safe to call more than once). Work already holding a
// turn of l — a grid read answering a calculated member by reading its
// siblings — goes on without waiting for another, so a tenant at its share
// never waits on itself. ctx ending first (the caller went away) returns its
// error and takes no turn.
func (l *Limiter) Acquire(ctx context.Context, key string) (context.Context, func(), error) {
	if held, _ := ctx.Value(heldKey{}).(*Limiter); held == l {
		return ctx, func() {}, nil
	}
	l.mu.Lock()
	if l.inUse < l.total && l.running[key] < l.perKey && len(l.waiting[key]) == 0 {
		l.take(key)
		l.mu.Unlock()
		return context.WithValue(ctx, heldKey{}, l), l.releaser(key), nil
	}
	l.seq++
	w := &waiter{ready: make(chan struct{}), seq: l.seq}
	l.waiting[key] = append(l.waiting[key], w)
	l.mu.Unlock()

	select {
	case <-w.ready:
		return context.WithValue(ctx, heldKey{}, l), l.releaser(key), nil
	case <-ctx.Done():
		l.mu.Lock()
		if w.granted {
			// Granted as ctx ended: hand the turn on.
			l.give(key)
		} else {
			q := l.waiting[key]
			for i, x := range q {
				if x == w {
					l.waiting[key] = append(q[:i:i], q[i+1:]...)
					break
				}
			}
			if len(l.waiting[key]) == 0 {
				delete(l.waiting, key)
			}
		}
		l.mu.Unlock()
		return ctx, func() {}, ctx.Err()
	}
}

func (l *Limiter) releaser(key string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.give(key)
			l.mu.Unlock()
		})
	}
}

// take counts a turn for key; l.mu is held.
func (l *Limiter) take(key string) {
	l.inUse++
	l.running[key]++
}

// give returns key's turn and hands free turns to the waiting keys holding
// the fewest; l.mu is held.
func (l *Limiter) give(key string) {
	l.inUse--
	if l.running[key]--; l.running[key] <= 0 {
		delete(l.running, key)
	}
	for l.inUse < l.total {
		next := ""
		for k, q := range l.waiting {
			if l.running[k] >= l.perKey {
				continue
			}
			if next == "" || l.running[k] < l.running[next] ||
				(l.running[k] == l.running[next] && q[0].seq < l.waiting[next][0].seq) {
				next = k
			}
		}
		if next == "" {
			return
		}
		q := l.waiting[next]
		w := q[0]
		if len(q) == 1 {
			delete(l.waiting, next)
		} else {
			l.waiting[next] = q[1:]
		}
		w.granted = true
		l.take(next)
		close(w.ready)
	}
}

var (
	defaultOnce sync.Once
	defaultL    *Limiter
)

// Default is the gateway's Limiter. HEAVY_WORK_CONCURRENCY sets its total
// turns (GRID_READ_CONCURRENCY, its earlier name, still does), by default
// twice the processors Go uses and at least 4; TENANT_WORK_SHARE sets one
// tenant's share, by default half the total.
func Default() *Limiter {
	defaultOnce.Do(func() {
		total := envInt("HEAVY_WORK_CONCURRENCY")
		if total == 0 {
			total = envInt("GRID_READ_CONCURRENCY")
		}
		if total == 0 {
			total = max(4, 2*runtime.GOMAXPROCS(0))
		}
		share := envInt("TENANT_WORK_SHARE")
		if share == 0 {
			share = max(1, total/2)
		}
		defaultL = New(total, share)
	})
	return defaultL
}

func envInt(name string) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
