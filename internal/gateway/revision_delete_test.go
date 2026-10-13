package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestRevisionDeleteQueuedBehindAnotherIsNotFound: deletes of one revision
// that queue behind another delete (a person clicking again while the
// first runs — 2026-10-13, six "revision.deleted" events for one revision)
// find nothing left once it commits. They answer 404 and record no
// deletion of their own.
func TestRevisionDeleteQueuedBehindAnotherIsNotFound(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	var revID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Doomed') RETURNING id::text`, f.modelID).Scan(&revID); err != nil {
		t.Fatal(err)
	}

	// The first delete, held open: it has the row, the requests wait for it.
	first, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx) //nolint:errcheck
	if _, err := first.Exec(ctx, `DELETE FROM model.revision WHERE id=$1::uuid`, revID); err != nil {
		t.Fatal(err)
	}

	statuses := make(chan int, 2)
	for range 2 {
		go func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, f.srv.URL+"/api/developer/revisions/"+revID, nil)
			req.Header.Set("X-Dev-User", "rollup-test-approver")
			req.Header.Set("X-App-Id", f.appID)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			_ = resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}

	// Both requests are waiting on the held row before it goes.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		if err := f.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE 'DELETE FROM model.revision%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of 2 deletes waiting on the held revision", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if got := <-statuses; got != http.StatusNotFound {
			t.Errorf("queued delete: status %d, want 404", got)
		}
	}
	var recorded int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM audit.audit_event WHERE event_type = 'revision.deleted' AND resource_id = $1`, revID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Errorf("%d revision.deleted events from deletes that removed nothing, want 0", recorded)
	}
}
