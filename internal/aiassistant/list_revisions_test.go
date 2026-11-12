package aiassistant_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// list_revisions failed on every call: its date column was a SQL date
// scanned into a string (2026-11-12, "can't scan into dest[2]").
func TestListRevisionsAnswers(t *testing.T) {
	ctx := context.Background()
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revA := seedRevision(t, pool, modelID, "Rev A")
	seedRevision(t, pool, modelID, "Rev B")
	out, err := aiassistant.NewToolExecutor(pool, modelID, revA).Execute(ctx, "list_revisions", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Rev A — "+time.Now().UTC().Format("2006-01")) || !strings.Contains(out, "Rev B") {
		t.Fatalf("list_revisions:\n%s", out)
	}
}
