package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestUnchangedReadAnswersNotModified: a grid read carries an ETag; a poll
// sending it back while nothing changed is answered 304 (the read itself
// changes nothing it depends on); another reader holding that tag gets the
// answer; and after each kind of change — a typed cell and the
// recalculation it starts, a metric edited, a member added, an access rule —
// the next poll is answered in full, and once the polls settle the answer
// shows the change: never the answer from before it.
func TestUnchangedReadAnswersNotModified(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	d.awaitCalc("margin", map[string]string{}, 760_000)

	scope, _ := json.Marshal(map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q1"})
	path := "/api/grid?revision_id=" + d.revID + "&grid_def_id=" + d.gridID + "&scope=" + url.QueryEscape(string(scope))

	type answer struct {
		status int
		tag    string
		body   []byte
	}
	get := func(persona, path, tag string) answer {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), "GET", d.srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Dev-User", persona)
		req.Header.Set("X-App-Id", d.appID)
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
			t.Fatalf("GET %s as %s: %d %s", path, persona, resp.StatusCode, body)
		}
		if resp.StatusCode == http.StatusNotModified && len(body) > 0 {
			t.Fatalf("a 304 carried a body: %s", body)
		}
		return answer{resp.StatusCode, resp.Header.Get("ETag"), body}
	}
	// settle polls as a console does until a poll is answered 304, and
	// returns the last full answer: the first poll after a change must be
	// a full one.
	settle := func(label, persona, path string, last answer) answer {
		t.Helper()
		first := true
		for i := 0; i < 80; i++ {
			a := get(persona, path, last.tag)
			if a.status == http.StatusNotModified {
				if first {
					t.Fatalf("%s: the first poll after it was answered 304, from before it", label)
				}
				if a.tag != last.tag {
					t.Fatalf("%s: a 304 named tag %s, the poll sent %s", label, a.tag, last.tag)
				}
				return last
			}
			first = false
			if a.tag == "" || a.tag == last.tag {
				t.Fatalf("%s: a full answer with tag %q after %q", label, a.tag, last.tag)
			}
			last = a
			time.Sleep(150 * time.Millisecond)
		}
		t.Fatalf("%s: the polls never settled", label)
		return last
	}
	totals := func(a answer) map[string]float64 {
		t.Helper()
		var g struct {
			Totals map[string]float64 `json:"totals"`
		}
		if err := json.Unmarshal(a.body, &g); err != nil {
			t.Fatalf("%v: %s", err, a.body)
		}
		return g.Totals
	}

	// The fixture's people do not include a tenant administrator, who adds
	// models: one is set up as the fixture sets up its people.
	var tenantAdmin string
	if err := d.pool.QueryRow(context.Background(), `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-tenant-admin', 'tenant@demo.co', 'Tina Tenant', $1::uuid) RETURNING id::text`, d.custID).Scan(&tenantAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(context.Background(), `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'tenant_admin')`, tenantAdmin); err != nil {
		t.Fatal(err)
	}
	first := get(d.dev, path, "")
	if first.status != http.StatusOK || !strings.HasPrefix(first.tag, `W/"`) {
		t.Fatalf("first read: status %d, tag %q", first.status, first.tag)
	}
	if again := get(d.dev, path, first.tag); again.status != http.StatusNotModified {
		t.Fatalf("an unchanged poll: status %d, want 304", again.status)
	}
	if other := get(d.admin, path, first.tag); other.status != http.StatusOK {
		t.Fatalf("another reader with the developer's tag: status %d, want the answer", other.status)
	}
	if got := totals(first)[d.metric["margin"]]; got != 200_000 {
		t.Fatalf("margin at UK/LAPTOP/Q1 = %v, want 200,000", got)
	}

	// A save in another model of the same application leaves this model's
	// polls answered 304: the generation is the model's own.
	other := d.call("POST", "/api/admin/models", "demo-tenant-admin", map[string]any{"application_id": d.appID, "name": "Other plan"})
	otherID, _ := other["id"].(string)
	var otherRev string
	if err := d.pool.QueryRow(context.Background(), `SELECT id::text FROM model.revision WHERE model_id = $1::uuid`, otherID).Scan(&otherRev); err != nil {
		t.Fatalf("the other model's revision: %v", err)
	}
	d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "channel", "revision_id": otherRev, "dimension_type": "standard"})
	if again := get(d.dev, path, first.tag); again.status != http.StatusNotModified {
		t.Fatalf("after a save in another model: status %d, want 304", again.status)
	}

	// A typed cell, and the recalculation it starts in the background.
	if st, raw := d.writeCell(d.dev, "revenue", "UK", "LAPTOP", "Q1", 950_000); st != http.StatusOK {
		t.Fatalf("write: %d %s", st, raw)
	}
	last := settle("a typed cell", d.dev, path, first)
	if got := totals(last)[d.metric["margin"]]; got != 250_000 {
		t.Fatalf("after revenue 950,000 the settled answer's margin is %v, want 250,000 (recalculated)", got)
	}

	// A metric edited (its label, part of the answer).
	d.call("PATCH", "/api/developer/metrics/"+d.metric["margin"], d.dev, map[string]any{"label": "Margin (edited)"})
	last = settle("a metric edited", d.dev, path, last)
	if !strings.Contains(string(last.body), "Margin (edited)") {
		t.Fatalf("the settled answer does not carry the edited label")
	}

	// A member added (the grid's member list).
	d.call("POST", "/api/developer/dimensions/"+d.geoDim+"/members", d.dev,
		map[string]any{"code": "FR", "label": "France", "parent_member_id": d.geo["EMEA"]})
	last = settle("a member added", d.dev, path, last)
	if !strings.Contains(string(last.body), `"FR"`) {
		t.Fatalf("the settled answer does not list the added member")
	}

	// An access rule: a business user's poll, then UK hidden from them.
	wide := "/api/grid?revision_id=" + d.revID + "&grid_def_id=" + d.gridID
	rep := get(d.westRep, wide, "")
	if !strings.Contains(string(rep.body), `"UK"`) {
		t.Fatalf("before the rule the business user's answer has no UK")
	}
	if again := get(d.westRep, wide, rep.tag); again.status != http.StatusNotModified {
		t.Fatalf("the business user's unchanged poll: status %d, want 304", again.status)
	}
	d.setAccessRules(d.westRepID, member(d.geo["UK"], "hidden"))
	rep = settle("an access rule", d.westRep, wide, rep)
	if strings.Contains(string(rep.body), `"UK"`) {
		t.Fatalf("after UK was hidden from the business user their settled answer still has it")
	}
}
