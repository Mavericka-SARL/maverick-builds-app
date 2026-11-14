package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// A table's re-posts run one at a time: a posting changed twice in quick
// succession re-applies twice in the background, and the first re-post's
// records landed after the second withdrew them — a record a new row filter
// left out came back (2026-11-14). A re-post waits while another of the same
// table holds its lock, and goes on when it is released.
func TestTableRepostWaitsForTheOneInFlight(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('table repost'), hashtext($1))`, f.formID); err != nil {
		t.Fatal(err)
	}
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := h.backfillFormMapping(ctx, f.mappingID, "")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a re-post ran (%v) while another re-post of its table held the lock", err)
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('table repost'), hashtext($1))`, f.formID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("re-post after the lock was released: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the re-post did not go on once the lock was released")
	}
}
