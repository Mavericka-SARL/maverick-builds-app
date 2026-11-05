package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestDeveloperSetsTheModelDateFormat: a developer sets how a model's dates
// read; /api/demo (what every console reads) and the developer's model list
// carry it, an unknown format is refused, and "" hands it back to each
// reader's browser.
func TestDeveloperSetsTheModelDateFormat(t *testing.T) {
	f := setupRoundTripFixture(t)
	dev := "rollup-test-approver"
	path := "/api/developer/models/" + f.modelID + "/date-format"
	demoFormat := func() string {
		t.Helper()
		status, raw := doAs(t, f.rollupFixture, "GET", "/api/demo", dev, f.appID, nil)
		if status != http.StatusOK {
			t.Fatalf("/api/demo: %d %s", status, raw)
		}
		var demo struct {
			DateFormat string `json:"date_format"`
		}
		_ = json.Unmarshal([]byte(raw), &demo)
		return demo.DateFormat
	}
	if got := demoFormat(); got != "" {
		t.Fatalf("a new model's date format = %q, want none", got)
	}
	if status, raw := doAs(t, f.rollupFixture, "PUT", path, dev, f.appID, map[string]any{"date_format": "DD/MM/YYYY"}); status != http.StatusOK {
		t.Fatalf("set DD/MM/YYYY: %d %s", status, raw)
	}
	if got := demoFormat(); got != "DD/MM/YYYY" {
		t.Errorf("/api/demo date_format = %q, want DD/MM/YYYY", got)
	}
	status, raw := doAs(t, f.rollupFixture, "GET", "/api/developer/applications", dev, f.appID, nil)
	if status != http.StatusOK || !strings.Contains(raw, `"date_format":"DD/MM/YYYY"`) {
		t.Errorf("developer model list: %d, want the model's date_format in %.300s", status, raw)
	}
	if status, raw := doAs(t, f.rollupFixture, "PUT", path, dev, f.appID, map[string]any{"date_format": "dd-mm-yy"}); status != http.StatusBadRequest {
		t.Errorf("an unknown format: %d %s, want 400", status, raw)
	}
	if status, raw := doAs(t, f.rollupFixture, "PUT", path, dev, f.appID, map[string]any{}); status != http.StatusBadRequest {
		t.Errorf("no date_format: %d %s, want 400", status, raw)
	}
	if got := demoFormat(); got != "DD/MM/YYYY" {
		t.Errorf("refused changes changed the format to %q", got)
	}
	if status, raw := doAs(t, f.rollupFixture, "PUT", path, dev, f.appID, map[string]any{"date_format": ""}); status != http.StatusOK {
		t.Fatalf("clear: %d %s", status, raw)
	}
	if got := demoFormat(); got != "" {
		t.Errorf("cleared date format = %q, want none", got)
	}
	if status, raw := doAs(t, f.rollupFixture, "PUT", "/api/developer/models/00000000-0000-0000-0000-000000000001/date-format", dev, f.appID, map[string]any{"date_format": "YYYY-MM-DD"}); status != http.StatusNotFound {
		t.Errorf("a model that is not there: %d %s, want 404", status, raw)
	}
}
