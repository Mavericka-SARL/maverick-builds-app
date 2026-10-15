package calculation

import (
	"slices"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// passQueue folds the recalculations asked of one revision while a pass
// runs into the next pass, instead of running one pass per request. Every
// write that moves a calculated value asks for a pass (a cell, an import, a
// member), and each pass recalculates everything those inputs reach: forty
// uploads of 200 rows each queued forty passes, and the grid's calculated
// values settled almost three minutes after the last upload had answered,
// although every pass but the last recalculated values the next one would
// replace. A pass that starts after a request was made sees that request's
// writes, so one pass serves every request that arrived before it began.
//
// A request returns once a pass that began after it has finished — the same
// promise a pass of its own gave — so a caller that waits for its
// recalculation still sees it done.
type passQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	running bool
	pending map[string]bool // metric ids the next pass calculates
	next    uint64          // the pass that takes pending when it starts
	done    uint64          // the last pass finished
	failed  map[uint64]error
}

type passQueueKey struct {
	pool                *pgxpool.Pool
	modelID, revisionID string
}

var passQueues sync.Map // passQueueKey → *passQueue

// queueFor is the pass queue of a revision on pool: requests share passes
// only on the same database, never with a store bound to a transaction
// (the AI Developer's dry run), whose writes are rolled back.
func queueFor(pool *pgxpool.Pool, modelID, revisionID string) *passQueue {
	key := passQueueKey{pool: pool, modelID: modelID, revisionID: revisionID}
	if q, ok := passQueues.Load(key); ok {
		return q.(*passQueue)
	}
	q := &passQueue{pending: map[string]bool{}, next: 1, failed: map[uint64]error{}}
	q.cond = sync.NewCond(&q.mu)
	v, _ := passQueues.LoadOrStore(key, q)
	return v.(*passQueue)
}

// run has metricIDs calculated by a pass that starts after this call: the
// caller runs it when no pass is running, and otherwise waits for the next
// pass, which one of the waiting callers runs with every metric asked for
// meanwhile. It returns the error of the pass that calculated metricIDs.
func (q *passQueue) run(metricIDs []string, pass func(metricIDs []string) error) error {
	q.mu.Lock()
	for _, id := range metricIDs {
		q.pending[id] = true
	}
	mine := q.next
	for q.done < mine {
		if q.running {
			q.cond.Wait()
			continue
		}
		q.running = true
		ids := make([]string, 0, len(q.pending))
		for id := range q.pending {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		q.pending = map[string]bool{}
		gen := q.next
		q.next++
		q.mu.Unlock()
		q.runPass(gen, ids, pass)
		q.mu.Lock()
	}
	err := q.failed[mine]
	q.mu.Unlock()
	return err
}

// runPass runs one pass and records it finished, even when it panics, so
// the revision's queue never waits on a pass that is gone.
func (q *passQueue) runPass(gen uint64, ids []string, pass func([]string) error) {
	var err error
	defer func() {
		q.mu.Lock()
		if err != nil {
			q.failed[gen] = err
		}
		// Keep the errors a waiter may still read; older ones go.
		for g := range q.failed {
			if g+64 < gen {
				delete(q.failed, g)
			}
		}
		q.done, q.running = gen, false
		q.cond.Broadcast()
		q.mu.Unlock()
	}()
	err = pass(ids)
}
