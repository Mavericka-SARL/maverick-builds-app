package calculation

import (
	"context"
	"time"
)

// InputValueMapSQL lets the external tests explain LoadInputValueMap's plan.
const InputValueMapSQL = inputValueMapSQL

// SetCopyRowsAt makes result sets of n rows or more go through COPY, until
// the returned func restores the default.
func SetCopyRowsAt(n int) (restore func()) {
	old := copyRowsAt
	copyRowsAt = n
	return func() { copyRowsAt = old }
}

// SetLeanCellsAt makes metrics of n cells or more take the lean path,
// until the returned func restores the default.
func SetLeanCellsAt(n int64) (restore func()) {
	old := leanCellsAt
	leanCellsAt = n
	return func() { leanCellsAt = old }
}

// HoldPassLock takes revisionID's pass lock in the database as another
// gateway's pass would, until release is called.
func HoldPassLock(s *Store, revisionID string) (release func(), err error) {
	l, err := s.lockPasses(context.Background(), revisionID)
	if err != nil {
		return nil, err
	}
	return l.release, nil
}

// SetPassLockPing checks held pass locks every d, until the returned func
// restores the default.
func SetPassLockPing(d time.Duration) (restore func()) {
	old := passLockPing
	passLockPing = d
	return func() { passLockPing = old }
}

// RunUnderPassLock runs pass as a recalculation pass holds its lock.
func RunUnderPassLock(s *Scheduler, revisionID string, pass func(context.Context) error) error {
	return s.underPassLock(context.Background(), revisionID, pass)
}
