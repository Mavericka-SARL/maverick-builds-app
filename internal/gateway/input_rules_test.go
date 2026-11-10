package gateway

// Input rules (migration 173; owner's decision 2026-11-09: a small generic
// rule set on input metrics, refusing writes). A developer gives a number,
// percentage or currency input a lowest and highest value and whole numbers
// only; every path that enters a value refuses one outside them — a typed
// cell, a batch (naming the cell), a file import (naming the row), an
// incremental load whose sum leaves the range (the database refuses it) —
// while a clear is always accepted, and a value stored before the rule, like
// the rules themselves, travels with a revision copy.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestInputRulesRefuseEnteredValues(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		return f.do(t, http.MethodPost, path, body)
	}
	mk := func(path string, body map[string]any) string {
		t.Helper()
		status, out := post(path, body)
		if status != http.StatusOK {
			t.Fatalf("POST %s: %d %v", path, status, out)
		}
		id, _ := out["id"].(string)
		return id
	}
	for _, code := range []string{"DE", "FR"} {
		mk(fmt.Sprintf("/api/developer/dimensions/%s/members", f.dimID), map[string]any{"code": code, "label": code})
	}
	metric := func(name string, input bool, format, formula string) string {
		return mk("/api/developer/metrics", map[string]any{"name": name, "is_input": input, "format": format, "formula": formula, "revision_id": f.revID})
	}
	units := metric("Units", true, "number", "")
	share := metric("Share", true, "percentage", "")
	note := metric("Note", true, "text", "")
	doubled := metric("Doubled", false, "number", "Units * 2")
	grid := mk("/api/developer/grids", map[string]any{"name": "Plan", "revision_id": f.revID})
	for _, p := range []string{"/dimensions/" + f.dimID, "/metrics/" + units, "/metrics/" + share, "/metrics/" + note, "/metrics/" + doubled} {
		if status, out := post("/api/developer/grids/"+grid+p, nil); status != http.StatusOK {
			t.Fatalf("assign %s: %d %v", p, status, out)
		}
	}
	write := func(metricID, code string, value float64) (int, map[string]any) {
		t.Helper()
		return post("/api/cells", map[string]any{"model_id": f.modelID, "revision_id": f.revID, "metric_id": metricID,
			"dim_codes": map[string]string{f.dimID: code}, "value": value})
	}
	stored := func(revisionID, metricName, code string) (float64, bool) {
		t.Helper()
		var v float64
		err := f.pool.QueryRow(ctx, `
			SELECT fi.value::float8 FROM runtime.fact_input fi JOIN model.metric_def m ON m.id = fi.metric_id
			JOIN model.dimension_def d ON d.revision_id = fi.revision_id AND d.name = 'FixtureRegion'
			WHERE fi.revision_id = $1::uuid AND m.name = $2 AND fi.dim_members->>d.id::text = $3
			ORDER BY fi.entered_at DESC, fi.id DESC LIMIT 1`, revisionID, metricName, code).Scan(&v)
		return v, err == nil
	}

	// A value stored before the rule existed.
	if status, out := write(units, "FR", -3); status != http.StatusOK {
		t.Fatalf("FR before the rule: %d %v", status, out)
	}

	patch := func(id string, body map[string]any) (int, map[string]any) {
		t.Helper()
		return f.do(t, http.MethodPatch, "/api/developer/metrics/"+id, body)
	}
	// Rules a metric cannot have are refused, saying why.
	for name, c := range map[string]struct {
		id    string
		body  map[string]any
		reply string
	}{
		"min above max":  {units, map[string]any{"name": "Units", "formula": "", "input_rules": map[string]any{"min": 5, "max": 1}}, "above max"},
		"unknown key":    {units, map[string]any{"name": "Units", "formula": "", "input_rules": map[string]any{"minimum": 0}}, "input_rules is"},
		"a calculation":  {doubled, map[string]any{"name": "Doubled", "formula": "Units * 2", "input_rules": map[string]any{"min": 0}}, "input metrics"},
		"a text's notes": {note, map[string]any{"name": "Note", "formula": "", "input_rules": map[string]any{"min": 0}}, "text metric"},
	} {
		if status, out := patch(c.id, c.body); status != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want 400", name, status, out)
		} else if msg, _ := out["error"].(string); !strings.Contains(msg, c.reply) {
			t.Errorf("%s: %q should say %q", name, msg, c.reply)
		}
	}
	if status, out := patch(units, map[string]any{"name": "Units", "formula": "", "input_rules": map[string]any{"min": 0, "whole": true}}); status != http.StatusOK {
		t.Fatalf("set Units rules: %d %v", status, out)
	}
	if status, out := patch(share, map[string]any{"name": "Share", "formula": "", "format": "percentage", "input_rules": map[string]any{"min": 0, "max": 100}}); status != http.StatusOK {
		t.Fatalf("set Share rules: %d %v", status, out)
	}
	// The grid answers them with the metric; a metric without any has none.
	if status, out := f.do(t, http.MethodGet, "/api/grid?grid_def_id="+grid+"&revision_id="+f.revID, nil); status != http.StatusOK {
		t.Fatalf("grid: %d %v", status, out)
	} else {
		raw, _ := json.Marshal(out["metrics"])
		if !strings.Contains(string(raw), `"input_rules":{"min":0,"whole":true}`) {
			t.Errorf("grid metrics lack Units' rules: %.600s", raw)
		}
	}

	// Typed cells: refused with the sentence, accepted inside the rules.
	for _, c := range []struct {
		metric, code string
		value        float64
		says         string
	}{
		{units, "DE", -5, "Units accepts values of at least 0, whole numbers only: -5 refused at DE"},
		{units, "DE", 2.5, "2.5 refused"},
		{share, "DE", 120, "Share accepts values from 0% to 100%: 120% refused at DE"},
	} {
		status, out := write(c.metric, c.code, c.value)
		msg, _ := out["error"].(string)
		if status != http.StatusUnprocessableEntity || !strings.Contains(msg, c.says) {
			t.Errorf("write %v: %d %v, want 422 saying %q", c.value, status, out, c.says)
		}
	}
	if v, ok := stored(f.revID, "Units", "DE"); ok {
		t.Errorf("a refused value was stored: DE = %v", v)
	}
	for _, c := range []struct {
		metric string
		value  float64
	}{{units, 4}, {share, 60}} {
		if status, out := write(c.metric, "DE", c.value); status != http.StatusOK {
			t.Errorf("write %v: %d %v, want 200", c.value, status, out)
		}
	}

	// A batch, dry run or not, names the refused cell and writes none.
	for _, dry := range []bool{true, false} {
		status, out := post("/api/cells/batch", map[string]any{"model_id": f.modelID, "revision_id": f.revID, "dry_run": dry,
			"cells": []map[string]any{
				{"metric_id": units, "dim_codes": map[string]string{f.dimID: "DE"}, "value": 3},
				{"metric_id": share, "dim_codes": map[string]string{f.dimID: "FR"}, "value": -1},
			}})
		raw, _ := json.Marshal(out["refused"])
		if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), `"index":1`) || !strings.Contains(string(raw), `"code":"INPUT_RULE"`) {
			t.Errorf("batch (dry run %v): %d %v, want cell 1 refused", dry, status, out)
		}
	}
	if v, _ := stored(f.revID, "Units", "DE"); v != 4 {
		t.Errorf("Units DE = %v after refused batches, want 4", v)
	}

	// A file import names the row and writes nothing.
	upload := func(csv, mode string) (int, map[string]any) {
		t.Helper()
		return post("/api/import/upload", map[string]string{"csv": csv, "revision_id": f.revID, "import_mode": mode})
	}
	if status, out := upload("FixtureRegion,Units\nDE,7\nFR,-2\n", "replace"); status != http.StatusUnprocessableEntity {
		t.Errorf("import with -2: %d %v, want 422", status, out)
	} else if raw, _ := json.Marshal(out); !strings.Contains(string(raw), "INPUT_RULE") || !strings.Contains(string(raw), "-2 refused at FR") {
		t.Errorf("import refusal should name the value: %s", raw)
	}
	if v, _ := stored(f.revID, "Units", "DE"); v != 4 {
		t.Errorf("Units DE = %v after the refused import, want 4", v)
	}
	// An incremental load adds to the cells: 60 + 50 leaves Share's range,
	// though 50 alone is inside it — the database refuses the sum.
	if status, out := upload("FixtureRegion,Share\nDE,50\n", "incremental"); status != http.StatusUnprocessableEntity {
		t.Errorf("incremental past the max: %d %v, want 422", status, out)
	} else if msg, _ := out["error"].(string); !strings.Contains(msg, "110% refused at DE") {
		t.Errorf("incremental refusal: %q should name the sum", msg)
	}
	if v, _ := stored(f.revID, "Share", "DE"); v != 60 {
		t.Errorf("Share DE = %v after the refused load, want 60", v)
	}
	if status, out := upload("FixtureRegion,Share\nDE,30\n", "incremental"); status != http.StatusOK {
		t.Errorf("incremental inside the max: %d %v", status, out)
	} else if v, _ := stored(f.revID, "Share", "DE"); v != 90 {
		t.Errorf("Share DE = %v, want 90", v)
	}

	// A revision copy carries the rules and the value stored before them.
	next := mk("/api/developer/revisions", map[string]any{"name": "Next", "source_revision_id": f.revID})
	var rules string
	if err := f.pool.QueryRow(ctx, `SELECT input_rules::text FROM model.metric_def WHERE revision_id=$1::uuid AND name='Units'`, next).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if rules != `{"min": 0, "whole": true}` {
		t.Errorf("Units' rules in the copy: %s", rules)
	}
	if v, ok := stored(next, "Units", "FR"); !ok || v != -3 {
		t.Errorf("FR in the copy = %v (%v), want -3 carried", v, ok)
	}

	// A clear is always accepted, even of a value the rules refuse.
	if status, out := post("/api/cells", map[string]any{"model_id": f.modelID, "revision_id": f.revID, "metric_id": units,
		"dim_codes": map[string]string{f.dimID: "FR"}, "clear": true}); status != http.StatusOK {
		t.Errorf("clear FR: %d %v", status, out)
	}

	// {} removes them; leaving the numeric formats drops them unasked.
	if status, out := patch(units, map[string]any{"name": "Units", "formula": "", "input_rules": map[string]any{}}); status != http.StatusOK {
		t.Fatalf("remove Units rules: %d %v", status, out)
	}
	if status, out := write(units, "DE", -5); status != http.StatusOK {
		t.Errorf("-5 without rules: %d %v", status, out)
	}
	if status, out := patch(share, map[string]any{"name": "Share", "formula": "", "format": "text", "drop_values": true}); status != http.StatusOK {
		t.Fatalf("Share to text: %d %v", status, out)
	}
	if err := f.pool.QueryRow(ctx, `SELECT input_rules::text FROM model.metric_def WHERE id=$1::uuid`, share).Scan(&rules); err != nil || rules != "{}" {
		t.Errorf("a text metric kept rules %s (%v)", rules, err)
	}
}
