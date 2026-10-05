package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A calculated metric of format text shows the text its formula gives — a
// key joining a member code and a pick-list's member, a status word — per
// cell, recalculated when an input changes; it is never added up, and no
// formula may read it.
func TestTextCalculations(t *testing.T) {
	f := setupRoundTripFixture(t)
	dev := "rollup-test-approver"
	rev := f.workingRevID
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
	must := func(method, path string, body any) string {
		t.Helper()
		status, raw := call(method, path, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		return out.ID
	}
	kind := must("POST", "/api/developer/dimensions", map[string]any{"name": "act_kind", "revision_id": rev})
	for _, c := range []string{"HIRE", "EXIT"} {
		must("POST", "/api/developer/dimensions/"+kind+"/members", map[string]any{"code": c, "label": c})
	}
	act := must("POST", "/api/developer/dimensions", map[string]any{"name": "act_row", "revision_id": rev})
	for _, c := range []string{"A1", "A2"} {
		must("POST", "/api/developer/dimensions/"+act+"/members", map[string]any{"code": c, "label": c})
	}
	actType := must("POST", "/api/developer/metrics", map[string]any{"name": "act_type", "is_input": true, "format": "picklist", "picklist_dimension_id": kind, "revision_id": rev})
	amount := must("POST", "/api/developer/metrics", map[string]any{"name": "act_amount", "is_input": true, "revision_id": rev})
	ref := must("POST", "/api/developer/metrics", map[string]any{"name": "act_ref", "is_input": true, "format": "text", "revision_id": rev})
	if status, raw := call("POST", "/api/developer/metrics", map[string]any{"name": "bad_key", "formula": `act_row & "|" & act_type`, "format": "text", "agg_rule": "sum", "revision_id": rev}); status != http.StatusBadRequest || !strings.Contains(raw, "never added up") {
		t.Errorf("a summed text: %d %s, want 400", status, raw)
	}
	key := must("POST", "/api/developer/metrics", map[string]any{"name": "act_key", "formula": `act_row & "|" & act_type`, "format": "text", "revision_id": rev})
	size := must("POST", "/api/developer/metrics", map[string]any{"name": "act_size", "formula": `IF(act_amount > 10, "Large", "Small")`, "format": "text", "revision_id": rev})
	if status, raw := call("POST", "/api/developer/metrics", map[string]any{"name": "reads_text", "formula": `LEN(act_key)`, "revision_id": rev}); status != http.StatusBadRequest || !strings.Contains(raw, "TEXT_METRIC_IN_FORMULA") {
		t.Errorf("a number formula reading a calculated text: %d %s, want TEXT_METRIC_IN_FORMULA", status, raw)
	}
	// A text calculation reads a text input's note and another text
	// calculation by name; not as a criteria range.
	refKey := must("POST", "/api/developer/metrics", map[string]any{"name": "act_ref_key", "formula": `act_ref & "|" & act_type`, "format": "text", "revision_id": rev})
	label := must("POST", "/api/developer/metrics", map[string]any{"name": "act_label", "formula": `act_key & " (" & act_size & ")"`, "format": "text", "revision_id": rev})
	if status, raw := call("POST", "/api/developer/metrics", map[string]any{"name": "ref_count", "formula": `IF(COUNTIFS(act_ref, "E1") > 0, "yes", "no")`, "format": "text", "revision_id": rev}); status != http.StatusBadRequest || !strings.Contains(raw, "TEXT_METRIC_IN_FORMULA") {
		t.Errorf("a text metric as a criteria range: %d %s, want TEXT_METRIC_IN_FORMULA", status, raw)
	}
	grid := must("POST", "/api/developer/grids", map[string]any{"name": "Actions", "revision_id": rev})
	must("POST", "/api/developer/grids/"+grid+"/dimensions/"+act, nil)
	for _, m := range []string{actType, amount, ref, key, size, refKey, label} {
		must("POST", "/api/developer/grids/"+grid+"/metrics/"+m, nil)
	}
	write := func(metric, row string, body map[string]any) {
		t.Helper()
		body["model_id"], body["revision_id"], body["metric_id"] = f.modelID, rev, metric
		body["dim_codes"] = map[string]string{act: row}
		if status, raw := call("POST", "/api/cells", body); status != http.StatusOK {
			t.Fatalf("write %s at %s: %d %s", metric, row, status, raw)
		}
	}
	write(actType, "A1", map[string]any{"member": "HIRE"})
	write(actType, "A2", map[string]any{"member": "EXIT"})
	write(amount, "A1", map[string]any{"value": 25})
	write(amount, "A2", map[string]any{"value": 4})
	write(ref, "A1", map[string]any{"text": "H101"})
	write(ref, "A2", map[string]any{"text": "E004"})

	read := func() map[string]string {
		t.Helper()
		_, raw := call("GET", "/api/grid?grid_def_id="+grid+"&model_id="+f.modelID+"&revision_id="+rev, nil)
		var g struct {
			Texts map[string]string `json:"texts"`
		}
		if err := json.Unmarshal([]byte(raw), &g); err != nil {
			t.Fatalf("grid: %v %.300s", err, raw)
		}
		return g.Texts
	}
	texts := read()
	for k, want := range map[string]string{key + ":A1": "A1|HIRE", key + ":A2": "A2|EXIT", size + ":A1": "Large", size + ":A2": "Small",
		refKey + ":A1": "H101|HIRE", refKey + ":A2": "E004|EXIT", label + ":A1": "A1|HIRE (Large)"} {
		if texts[k] != want {
			t.Errorf("texts[%s] = %q, want %q", k, texts[k], want)
		}
	}
	// The text follows its inputs.
	write(actType, "A2", map[string]any{"member": "HIRE"})
	write(amount, "A2", map[string]any{"value": 40})
	write(ref, "A2", map[string]any{"text": "E012"})
	texts = read()
	if texts[key+":A2"] != "A2|HIRE" || texts[size+":A2"] != "Large" || texts[refKey+":A2"] != "E012|HIRE" || texts[label+":A2"] != "A2|HIRE (Large)" {
		t.Errorf("after the inputs changed: key %q size %q ref key %q label %q, want A2|HIRE, Large, E012|HIRE, A2|HIRE (Large)",
			texts[key+":A2"], texts[size+":A2"], texts[refKey+":A2"], texts[label+":A2"])
	}
}
