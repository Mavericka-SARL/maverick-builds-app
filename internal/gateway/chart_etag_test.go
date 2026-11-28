package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestUnchangedChartAnswersNotModified: a chart's data carries an ETag, as a
// grid read does; a poll sending it back while nothing changed is answered
// 304 before the chart is worked out (six charts of one dashboard were
// worked out again every two seconds, 2026-11-27). Another context, the
// widget's settings changed, or a value typed: the next poll is answered in
// full.
func TestUnchangedChartAnswersNotModified(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	dash := idOf(d.call("POST", "/api/developer/dashboards", d.dev, map[string]any{"name": "Board", "revision_id": d.revID}))
	props := func(stacked bool) map[string]any {
		return map[string]any{"chart": map[string]any{"chart_type": "bar", "dimension_id": d.periodDim, "metric_ids": []string{d.metric["revenue"]},
			"context_defaults": map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP"}, "hide_rollup_members": true, "stacked": stacked}}
	}
	chart := idOf(d.call("POST", "/api/developer/dashboards/"+dash+"/widgets", d.dev, map[string]any{
		"widget_type": "chart", "ref_id": d.gridID, "widget_props": props(false)}))

	type answer struct {
		status int
		tag    string
		body   []byte
	}
	post := func(ctxt map[string]string, tag string) answer {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"context": ctxt})
		req, err := http.NewRequestWithContext(context.Background(), "POST", d.srv.URL+"/api/dashboard-widgets/"+chart+"/chart-data", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Dev-User", d.dev)
		req.Header.Set("X-App-Id", d.appID)
		req.Header.Set("Content-Type", "application/json")
		if tag != "" {
			req.Header.Set("If-None-Match", tag)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
			t.Fatalf("chart data: %d %s", resp.StatusCode, body)
		}
		return answer{resp.StatusCode, resp.Header.Get("ETag"), body}
	}
	uk := map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP"}
	first := post(uk, "")
	if first.status != http.StatusOK || first.tag == "" {
		t.Fatalf("first poll: %d, tag %q — want a full answer with a tag", first.status, first.tag)
	}
	if again := post(uk, first.tag); again.status != http.StatusNotModified || len(again.body) != 0 {
		t.Fatalf("unchanged poll: %d with %d bytes, want 304 and nothing more", again.status, len(again.body))
	}
	// Another context is another answer.
	de := map[string]string{d.geoDim: "DE", d.prodDim: "LAPTOP"}
	if other := post(de, first.tag); other.status != http.StatusOK || other.tag == first.tag {
		t.Errorf("another context with the first tag: %d, tag %q — want a full answer under another tag", other.status, other.tag)
	}
	// The widget's settings changed: a full answer.
	if st, raw := d.req("PATCH", "/api/developer/dashboards/"+dash+"/widgets/"+chart, d.dev, map[string]any{"widget_props": props(true)}); st != http.StatusOK {
		t.Fatalf("change the chart: %d %s", st, raw)
	}
	changed := post(uk, first.tag)
	if changed.status != http.StatusOK {
		t.Errorf("after the chart's settings changed: %d, want a full answer", changed.status)
	}
	// A value typed: the poll after it is a full one.
	if st, raw := d.writeCell(d.dev, "revenue", "UK", "LAPTOP", "Q1", 950_000); st != http.StatusOK {
		t.Fatalf("write: %d %s", st, raw)
	}
	if after := post(uk, changed.tag); after.status != http.StatusOK {
		t.Errorf("after a value was typed: %d, want a full answer", after.status)
	}
}
