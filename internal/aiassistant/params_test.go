package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// Every write tool refuses a key it does not read, naming it, instead of
// dropping it: a step that did less than it said used to pass the plan check
// (create_grid's "metrics" built every grid empty). revision_id is the one
// key every tool takes and ignores: the session decides the revision.
func TestEveryWriteToolRefusesAKeyItDoesNotRead(t *testing.T) {
	ctx := context.Background()
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	for _, tool := range aiassistant.WriteToolNames {
		_, _, err := exec.Execute(ctx, tool, []byte(`{"revision_id":"`+revID+`","zzz_invented":1}`))
		if err == nil || !strings.Contains(err.Error(), `does not read "zzz_invented"`) {
			t.Errorf("%s: err = %v, want the invented key refused by name", tool, err)
		}
	}
	// A nested key is checked too: a member of create_dimension.
	_, _, err := exec.Execute(ctx, "create_dimension", []byte(`{"name":"region","members":[{"code":"EU","lable":"Europe"}]}`))
	if err == nil || !strings.Contains(err.Error(), `"lable"`) {
		t.Errorf("a misspelt member key: err = %v", err)
	}
}
