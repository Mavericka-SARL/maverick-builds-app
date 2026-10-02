package gateway

// The record list fills each page with records the caller may see and pages
// on by cursor: records an access rule withholds neither fill a page nor
// end the list early (visibleRecordPage).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

func TestFormRecordListPagesVisibleRecords(t *testing.T) {
	f := setupSalesDemo(t)
	f.setAccessRules(f.roID, member(f.geo["AMER"], "hidden"))
	form := idOf(f.call("POST", "/api/forms", f.dev, map[string]any{
		"name": "deal", "label": "Deal",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "type": "number"},
			{"name": "region", "label": "Region", "type": "dimension", "dimension_id": f.geoDim},
		},
	}))
	// 150 newest deals in the US (hidden from Reed), 120 older in the UK; a
	// pair of records shares each timestamp, as an import's do.
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO runtime.form_record (form_id, data, status, created_at, updated_at)
		SELECT $1::uuid,
		       jsonb_build_object('amount', i, 'region', CASE WHEN i <= 150 THEN 'US' ELSE 'UK' END),
		       'submitted', now() - make_interval(secs => (i / 2)), now()
		FROM generate_series(1, 270) AS i`, form); err != nil {
		t.Fatal(err)
	}
	var before int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, form).Scan(&before)

	t.Run("the record list pages past records the person may not see", func(t *testing.T) {
		seen := map[string]bool{}
		cursor := ""
		for page := 0; page < 10; page++ {
			q := url.Values{"limit": {"100"}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", f.srv.URL+"/api/forms/"+form+"/records?"+q.Encode(), nil)
			req.Header.Set("X-Dev-User", f.ro)
			req.Header.Set("X-App-Id", f.appID)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var recs []struct {
				ID   string         `json:"id"`
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&recs)
			_ = resp.Body.Close()
			if page == 0 && len(recs) != 100 {
				t.Errorf("first page for Reed has %d records, want 100 (the newest 150 are hidden from him)", len(recs))
			}
			for _, r := range recs {
				if r.Data["region"] != "UK" {
					t.Errorf("Reed was served a %v record", r.Data["region"])
				}
				if seen[r.ID] {
					t.Errorf("record %s served twice", r.ID)
				}
				seen[r.ID] = true
			}
			cursor = resp.Header.Get("X-Next-Cursor")
			if cursor == "" {
				break
			}
		}
		if len(seen) != 120 {
			t.Errorf("Reed reached %d records, want all 120 he may see", len(seen))
		}
	})

	var after int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, form).Scan(&after)
	if after != before {
		t.Errorf("reading changed the form: %d records before, %d after", before, after)
	}
}
