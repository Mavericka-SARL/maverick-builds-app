package gateway

import (
	"context"
	"encoding/json"
	"testing"
)

// A table that keeps a dimension fills the dimension's declared properties
// from its fields of the same name: declaring reporting_line on gl_account
// gives every account the value of the GL map's "Reporting line" field, and
// a record added or changed later carries its value along. The AI build's
// Actual P&L read {GL Account}.reporting_line from a dimension whose
// mapping lived only in the table (2026-11-08).
func TestKeptDimensionPropertiesFollowTheTableFields(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	glDim := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "gl_account", "label": "GL Account", "revision_id": d.revID}))
	form := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{"name": "gl_map", "label": "GL Mapping", "fields": []map[string]any{
		{"name": "code", "label": "Account", "type": "autocode", "code_prefix": "GL-", "code_digits": 4, "core": true},
		{"name": "GL description", "label": "GL description", "type": "text", "required": true},
		{"name": "Reporting line", "label": "Reporting line", "type": "text"},
	}, "keeps_dimension_id": glDim, "member_label_field": "GL description", "edit_access": "everyone"}))
	create := func(desc, line string) string {
		t.Helper()
		raw, _ := json.Marshal(d.call("POST", "/api/forms/"+form+"/records", d.dev, map[string]any{"data": map[string]any{"GL description": desc, "Reporting line": line}}))
		var r struct {
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(raw, &r)
		return r.ID
	}
	property := func(desc string) string {
		t.Helper()
		var v string
		_ = d.pool.QueryRow(ctx, `SELECT COALESCE(properties->>'reporting_line', '') FROM model.dimension_member WHERE dimension_id=$1::uuid AND label=$2`, glDim, desc).Scan(&v)
		return v
	}
	create("Brand deals", "Sponsorships")
	ads := create("Programmatic ads", "Advertising")

	d.call("POST", "/api/developer/dimensions/"+glDim+"/properties", d.dev, map[string]any{"name": "reporting_line", "data_type": "text"})
	if got := property("Brand deals"); got != "Sponsorships" {
		t.Errorf("declared: Brand deals reads %q, want Sponsorships", got)
	}

	create("Affiliate", "Affiliate")
	if got := property("Affiliate"); got != "Affiliate" {
		t.Errorf("a record added later: %q", got)
	}
	d.call("PUT", "/api/records/"+ads, d.dev, map[string]any{"data": map[string]any{"GL description": "Programmatic ads", "Reporting line": "Sponsorships"}})
	if got := property("Programmatic ads"); got != "Sponsorships" {
		t.Errorf("a record changed: %q, want Sponsorships", got)
	}
	d.call("PUT", "/api/records/"+ads, d.dev, map[string]any{"data": map[string]any{"GL description": "Programmatic ads", "Reporting line": ""}})
	if got := property("Programmatic ads"); got != "" {
		t.Errorf("a field emptied leaves the property %q", got)
	}
}
