package aiassistant

import (
	"encoding/json"
	"testing"
)

func stepsOf(params ...string) []ProposalStep {
	out := make([]ProposalStep, len(params))
	for i, p := range params {
		out[i] = ProposalStep{Tool: "t", Params: json.RawMessage(p)}
	}
	return out
}

func TestDropStepsRenumbersAndDropsDependents(t *testing.T) {
	steps := stepsOf(
		`{"name":"a"}`,                        // 1
		`{"name":"b"}`,                        // 2 dropped
		`{"metric_id":"<created in step 2>"}`, // 3 uses 2: dropped too
		`{"metric_id":"<created in step 4>"}`, // 4 (self ref is odd but kept)
		`{"grid_id":"<step 1 id>","metric_id":"<created in step 4>"}`, // 5
	)
	out, gone := DropSteps(steps, map[int]bool{1: true})
	if len(out) != 3 || !gone[1] || !gone[2] {
		t.Fatalf("kept %d, gone %v; want 3 kept, steps 2 and 3 gone", len(out), gone)
	}
	if got := string(out[2].Params); got != `{"grid_id":"<step 1 id>","metric_id":"<created in step 2>"}` {
		t.Fatalf("renumbered = %s", got)
	}
}

func TestInsertStepsRenumbers(t *testing.T) {
	steps := stepsOf(`{"name":"a"}`, `{"name":"b"}`, `{"metric_id":"<created in step 2>"}`)
	out := InsertSteps(steps, []InsertedStep{{After: 0, Step: ProposalStep{Tool: "add_grid_metric", Params: json.RawMessage(`{"metric_id":"<created in step 1>"}`)}}})
	if len(out) != 4 || out[1].Tool != "add_grid_metric" {
		t.Fatalf("got %+v", out)
	}
	if got := string(out[3].Params); got != `{"metric_id":"<created in step 3>"}` {
		t.Fatalf("renumbered = %s", got)
	}
	if got := string(out[1].Params); got != `{"metric_id":"<created in step 1>"}` {
		t.Fatalf("inserted = %s", got)
	}
}
