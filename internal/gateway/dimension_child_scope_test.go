package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// requireResourceAccess on /api/developer/dimensions/{dimId}/... vouches only
// for {dimId}. A member or property addressed by its own id under that path
// must belong to that dimension: pairing another tenant's member or property
// id with a dimension of one's own must neither change nor delete it.
func TestDimensionChildrenScopedToPathDimension(t *testing.T) {
	f := setupAuthzScopeFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	dimA := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,'region','standard') RETURNING id::text`, f.modelAID, f.revAID)
	propA := q(`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid,'segment','text') RETURNING id::text`, dimA)
	memberA := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'EMEA','EMEA') RETURNING id::text`, dimA)

	dimB := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,'region','standard') RETURNING id::text`, f.modelBID, f.revBID)
	propB := q(`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid,'secret','text') RETURNING id::text`, dimB)
	memberB := q(`INSERT INTO model.dimension_member (dimension_id, code, label, properties) VALUES ($1::uuid,'SECRET','Secret','{"k":"v"}') RETURNING id::text`, dimB)

	call := func(method, path string, body any) int {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, f.srv.URL+path, &buf)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Dev-User", "authz-dev-a")
		req.Header.Set("X-App-Id", f.appAID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		return resp.StatusCode
	}
	base := "/api/developer/dimensions/" + dimA

	// Foreign member through the own dimension: PATCH and DELETE are 404
	// and the row is untouched.
	if s := call("PATCH", base+"/members/"+memberB, map[string]any{"code": "PWNED", "label": "PWNED", "properties": map[string]string{"evil": "1"}}); s != http.StatusNotFound {
		t.Errorf("PATCH foreign member: status %d, want 404", s)
	}
	if s := call("DELETE", base+"/members/"+memberB, nil); s != http.StatusNotFound {
		t.Errorf("DELETE foreign member: status %d, want 404", s)
	}
	var code, props string
	if err := f.pool.QueryRow(ctx, `SELECT code, properties::text FROM model.dimension_member WHERE id=$1::uuid`, memberB).Scan(&code, &props); err != nil {
		t.Fatalf("foreign member is gone: %v", err)
	}
	if code != "SECRET" || props != `{"k": "v"}` {
		t.Errorf("foreign member changed: code=%q properties=%s", code, props)
	}

	// Foreign property through the own dimension: DELETE is 404 and the
	// declaration survives.
	if s := call("DELETE", base+"/properties/"+propB, nil); s != http.StatusNotFound {
		t.Errorf("DELETE foreign property: status %d, want 404", s)
	}
	var propLeft bool
	if err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM model.dimension_property WHERE id=$1::uuid)`, propB).Scan(&propLeft); err != nil || !propLeft {
		t.Errorf("foreign property was deleted (err %v)", err)
	}

	// The developer's own member and property still work through the same
	// routes.
	if s := call("PATCH", base+"/members/"+memberA, map[string]any{"code": "EMEA", "label": "Europe", "properties": map[string]string{"segment": "core"}}); s != http.StatusOK {
		t.Errorf("PATCH own member: status %d, want 200", s)
	}
	if s := call("DELETE", base+"/properties/"+propA, nil); s != http.StatusOK {
		t.Errorf("DELETE own property: status %d, want 200", s)
	}
	if s := call("DELETE", base+"/members/"+memberA, nil); s != http.StatusOK {
		t.Errorf("DELETE own member: status %d, want 200", s)
	}
}
