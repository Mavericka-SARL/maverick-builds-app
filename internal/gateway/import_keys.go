package gateway

// An import's answer reaching the client, and an import sent again not
// being applied twice.
//
// An import that ran past the server's two-minute WriteTimeout was cut off
// after it had committed: the client saw the connection close (a 502 behind
// the ingress) and sent the file again, and an incremental import added
// every value a second time. Reported live (2026-10-14): 8,000 rows, each
// load of the same file slower than the last, the fifth one cut at 121 s
// with its rows saved.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// importDeadline bounds reading an import's file and writing its answer, in
// place of the server's ReadTimeout (60 s) and WriteTimeout (120 s): a large
// file over a slow link takes longer than a minute to arrive.
const importDeadline = 10 * time.Minute

// extendImportDeadlines gives an import request importDeadline to arrive and
// to answer. A writer that cannot take a deadline (a test recorder) is left
// as it is.
func extendImportDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(importDeadline))
	_ = rc.SetWriteDeadline(time.Now().Add(importDeadline))
}

// importIdempotencyKey reads the request's Idempotency-Key header ("" when
// it has none); ok is false when the key was refused and answered.
func importIdempotencyKey(w http.ResponseWriter, r *http.Request) (key string, ok bool) {
	key = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > 255 {
		jsonErr(w, fmt.Errorf("Idempotency-Key is longer than 255 characters"), http.StatusBadRequest)
		return "", false
	}
	return key, true
}

// replayKeyedImport answers an import userID already sent for modelID under
// key and reports false; true when the request is to run. A committed
// import's answer is given again (with "replayed": true) and nothing is
// applied. One still running answers 409. One that never finished — cut off
// before its commit, so nothing of it was saved — frees the key and runs.
func (h *handler) replayKeyedImport(w http.ResponseWriter, ctx context.Context, store *importpkg.Store, modelID, userID, key string) bool {
	prior, err := store.JobByKey(ctx, modelID, userID, key)
	if err != nil {
		jsonErr(w, fmt.Errorf("look up the idempotency key: %w", err), http.StatusInternalServerError)
		return false
	}
	switch {
	case prior == nil:
		return true
	case prior.Status == "committed":
		jsonOK(w, map[string]any{
			"job_id":      prior.ID,
			"status":      "committed",
			"total_rows":  prior.TotalRows,
			"valid_rows":  prior.TotalRows,
			"error_rows":  0,
			"revision_id": prior.RevisionID,
			"replayed":    true,
		})
		return false
	case (prior.Status == "validating" || prior.Status == "staged") && time.Since(prior.CreatedAt) < importDeadline:
		jsonErr(w, importpkg.ErrImportKeyInUse, http.StatusConflict)
		return false
	}
	if err := store.ReleaseKey(ctx, prior.ID); err != nil {
		jsonErr(w, fmt.Errorf("release the idempotency key: %w", err), http.StatusInternalServerError)
		return false
	}
	return true
}
