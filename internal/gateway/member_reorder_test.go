package gateway

// PUT /api/developer/dimensions/{id}/members/order: a developer sets the
// order of one level of a dimension's members, and every reader that sorts
// by sort_order — the grid, GET /api/dimensions — shows it. Only the
// developer role reaches it; the order stays inside the revision and travels
// with revision duplication and model export/import.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/modeledit"
)

func TestReorderDimensionMembers_Route(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	const dev = "mm-dev"
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}

	// Org: R1 {C1, C2}, R2 {C3}, R3.
	dimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Org') RETURNING id::text`, f.modelA, f.revA)
	member := func(code string, parent *string, sortOrder int) string {
		return q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order)
			VALUES ($1::uuid, $2, $2, $3::uuid, $4) RETURNING id::text`, dimID, code, parent, sortOrder)
	}
	r1, r2, r3 := member("R1", nil, 1), member("R2", nil, 2), member("R3", nil, 3)
	c1, c2, c3 := member("C1", &r1, 4), member("C2", &r1, 5), member("C3", &r2, 6)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Org grid') RETURNING id::text`, f.modelA, f.revA)
	q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) RETURNING id::text`, grid, dimID)

	path := "/api/developer/dimensions/" + dimID + "/members/order"
	put := func(sub, p string, body any) (int, string) {
		t.Helper()
		code, raw := f.do(t, "PUT", p, sub, f.app1, f.modelA, body)
		return code, string(raw)
	}
	order := func(dim string) string {
		t.Helper()
		return q(`SELECT string_agg(code, ',' ORDER BY sort_order, code) FROM model.dimension_member WHERE dimension_id=$1::uuid`, dim)
	}
	gridOrder := func() string {
		t.Helper()
		code, raw := f.do(t, "GET", "/api/grid?grid_def_id="+grid, dev, f.app1, f.modelA, nil)
		if code != http.StatusOK {
			t.Fatalf("GET /api/grid: %d %s", code, raw)
		}
		var g struct {
			Dimensions []struct {
				ID      string `json:"id"`
				Members []struct {
					Code string `json:"code"`
				} `json:"members"`
			} `json:"dimensions"`
		}
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatalf("decode grid: %v", err)
		}
		for _, d := range g.Dimensions {
			if d.ID == dimID {
				var codes []string
				for _, m := range d.Members {
					codes = append(codes, m.Code)
				}
				return strings.Join(codes, ",")
			}
		}
		t.Fatalf("dimension not on the grid: %s", raw)
		return ""
	}
	dimensionsOrder := func() string {
		t.Helper()
		code, raw := f.do(t, "GET", "/api/dimensions", dev, f.app1, f.modelA, nil)
		if code != http.StatusOK {
			t.Fatalf("GET /api/dimensions: %d %s", code, raw)
		}
		return strings.Join(memberCodes(t, raw, dimID), ",")
	}

	// ── the developer reorders the top level, then a nested level ────────
	if code, raw := put(dev, path, map[string]any{"parent_member_id": nil, "member_ids": []string{r3, r1, r2}}); code != http.StatusOK {
		t.Fatalf("reorder the top level: %d %s", code, raw)
	}
	want := "R3,R1,C1,C2,R2,C3"
	if got := order(dimID); got != want {
		t.Errorf("stored order %s, want %s (tree order: each member followed by its children)", got, want)
	}
	if got := q(`SELECT string_agg(sort_order::text, ',' ORDER BY sort_order) FROM model.dimension_member WHERE dimension_id=$1::uuid`, dimID); got != "1,2,3,4,5,6" {
		t.Errorf("sort_order %s, want renumbered 1..6", got)
	}
	if code, raw := put(dev, path, map[string]any{"parent_member_id": r1, "member_ids": []string{c2, c1}}); code != http.StatusOK {
		t.Fatalf("reorder R1's children: %d %s", code, raw)
	}
	want = "R3,R1,C2,C1,R2,C3"
	if got := order(dimID); got != want {
		t.Errorf("stored order %s, want %s", got, want)
	}
	if got := gridOrder(); got != want {
		t.Errorf("grid shows %s, want %s", got, want)
	}
	if got := dimensionsOrder(); got != want {
		t.Errorf("GET /api/dimensions shows %s, want %s", got, want)
	}

	// ── one audit event, naming the dimension, the parent and the order ──
	var resourceID, revisionID, appID string
	var rawMeta []byte
	if err := f.pool.QueryRow(ctx, `
		SELECT resource_id, COALESCE(revision_id::text,''), COALESCE(application_id::text,''), metadata
		FROM audit.audit_event WHERE event_type='dimension.members_reordered' ORDER BY occurred_at DESC LIMIT 1`,
	).Scan(&resourceID, &revisionID, &appID, &rawMeta); err != nil {
		t.Fatalf("no dimension.members_reordered audit event: %v", err)
	}
	var meta map[string]string
	_ = json.Unmarshal(rawMeta, &meta)
	if resourceID != dimID || revisionID != f.revA || appID != f.app1 ||
		meta["parent_member_id"] != r1 || meta["parent_code"] != "R1" ||
		meta["member_ids"] != c2+","+c1 || meta["codes"] != `["C2","C1"]` {
		t.Errorf("audit event: resource %s revision %s app %s metadata %v", resourceID, revisionID, appID, meta)
	}
	if n := q(`SELECT count(*)::text FROM audit.audit_event WHERE event_type='dimension.members_reordered'`); n != "2" {
		t.Errorf("%s audit events for two reorders, want 2", n)
	}

	// ── a wrong list is a 400 that names the problem; nothing moves ──────
	months := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, 'Months', 'time', 'month', 1) RETURNING id::text`, f.modelA, f.revA)
	jan := q(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index)
		VALUES ($1::uuid, 'JAN', 'Jan', '2026-01-01', '2026-01-31', 0) RETURNING id::text`, months)
	for _, tc := range []struct {
		what string
		path string
		body any
		want string
	}{
		{"a member left out", path, map[string]any{"member_ids": []string{r3, r1}}, "missing: R2"},
		{"a member twice", path, map[string]any{"member_ids": []string{r3, r1, r1, r2}}, "listed more than once"},
		{"another model's member", path, map[string]any{"member_ids": []string{r3, r1, r2, f.memberB}}, "not a member of this dimension"},
		{"a child among the top level", path, map[string]any{"member_ids": []string{r3, r1, r2, c3}}, "not one of the top-level members"},
		{"a top-level member among R1's children", path, map[string]any{"parent_member_id": r1, "member_ids": []string{c2, c1, r2}}, `not one of the children of \"R1\"`},
		{"another model's parent", path, map[string]any{"parent_member_id": f.memberB, "member_ids": []string{}}, "not a member of this dimension"},
		{"a malformed id", path, map[string]any{"member_ids": []string{r3, r1, "not-a-uuid"}}, "not a member of this dimension"},
		{"no member_ids", path, map[string]any{"parent_member_id": nil}, "member_ids is required"},
		{"a time dimension", "/api/developer/dimensions/" + months + "/members/order", map[string]any{"member_ids": []string{jan}}, "time periods keep calendar order"},
	} {
		code, raw := put(dev, tc.path, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(raw, tc.want) {
			t.Errorf("%s: %d %s, want 400 naming %q", tc.what, code, raw, tc.want)
		}
	}
	if got := order(dimID); got != want {
		t.Errorf("a refused reorder moved members: %s, want %s", got, want)
	}

	// ── only the developer role reaches it ───────────────────────────────
	q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		SELECT 'mm-ta-only', 'mm-ta-only@mm.test', 'TA', customer_id FROM core.application WHERE id=$1::uuid RETURNING id::text`, f.app1)
	q(`INSERT INTO identity.role_assignment (user_id, role) SELECT id, 'tenant_admin' FROM identity.user WHERE keycloak_sub='mm-ta-only' RETURNING role::text`)
	for _, sub := range []string{"mm-ba1", "mm-user", "mm-ta-only", "mm-tenant-admin"} {
		if code, raw := put(sub, path, map[string]any{"member_ids": []string{r1, r2, r3}}); code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", sub, code, raw)
		}
	}

	// ── another tenant's dimension, or none, is a 404 ────────────────────
	theirs := q(`SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid LIMIT 1`, f.modelD)
	theirMember := q(`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid LIMIT 1`, theirs)
	for _, p := range []string{theirs, "00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if code, raw := put(dev, "/api/developer/dimensions/"+p+"/members/order", map[string]any{"member_ids": []string{theirMember}}); code != http.StatusNotFound {
			t.Errorf("dimension %s: %d %s, want 404", p, code, raw)
		}
	}
	if got := q(`SELECT sort_order::text FROM model.dimension_member WHERE id=$1::uuid`, theirMember); got != "0" {
		t.Errorf("another tenant's member sort_order changed to %s", got)
	}
	if got := order(dimID); got != want {
		t.Errorf("a refused caller moved members: %s, want %s", got, want)
	}

	// ── a duplicated revision carries the order, and stays apart ─────────
	code, raw := f.do(t, "POST", "/api/developer/revisions", dev, f.app1, f.modelA, map[string]any{"name": "Copy", "source_revision_id": f.revA})
	if code != http.StatusOK {
		t.Fatalf("duplicate the revision: %d %s", code, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &created)
	copyDim := q(`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='Org'`, created.ID)
	if got := order(copyDim); got != want {
		t.Errorf("duplicated revision's order %s, want %s", got, want)
	}
	copyRoots := strings.Split(q(`SELECT string_agg(id::text, ',' ORDER BY code DESC) FROM model.dimension_member WHERE dimension_id=$1::uuid AND parent_member_id IS NULL`, copyDim), ",")
	if code, raw := put(dev, "/api/developer/dimensions/"+copyDim+"/members/order", map[string]any{"member_ids": copyRoots}); code != http.StatusOK {
		t.Fatalf("reorder the copy: %d %s", code, raw)
	}
	if got := order(copyDim); got != "R3,R2,C3,R1,C2,C1" {
		t.Errorf("copy after its reorder: %s", got)
	}
	if got := order(dimID); got != want {
		t.Errorf("reordering the copy moved the source revision's members: %s, want %s", got, want)
	}

	// ── model export and import carry it ─────────────────────────────────
	code, raw = f.do(t, "GET", "/api/admin/models/"+f.modelA+"/export?revision_id="+f.revA, "mm-tenant-admin", f.app1, "", nil)
	if code != http.StatusOK {
		t.Fatalf("export: %d %.300s", code, raw)
	}
	code, raw = f.do(t, "POST", "/api/admin/models/import", "mm-tenant-admin", f.app2, "",
		map[string]any{"application_id": f.app2, "model_name": "Imported", "package": json.RawMessage(raw)})
	if code != http.StatusOK {
		t.Fatalf("import: %d %s", code, raw)
	}
	var imported struct {
		RevisionID string `json:"revision_id"`
	}
	_ = json.Unmarshal(raw, &imported)
	importedDim := q(`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='Org'`, imported.RevisionID)
	if got := order(importedDim); got != want {
		t.Errorf("imported order %s, want %s", got, want)
	}
}

// The route answers 404 where the other member edits answer 403, so it
// checks scope with its own copy of requireResourceAccess. Each caller and
// dimension here is asked of both — the reorder and GET …/properties, which
// goes through requireResourceAccess — and the two must agree: reorder 200
// where the properties read is 200, 404 where it is 403 or 404.
func TestReorderDimensionMembers_ScopeMatchesMemberEdits(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	dimOf := func(name string) (dim, roots string) {
		t.Helper()
		dim = q(`SELECT id::text FROM model.dimension_def WHERE name=$1`, name)
		roots = q(`SELECT string_agg(id::text, ',' ORDER BY sort_order, code) FROM model.dimension_member
			WHERE dimension_id=$1::uuid AND parent_member_id IS NULL`, dim)
		return dim, roots
	}
	// A developer of ws1a narrowed by identity.user_model_access to model A.
	narrow := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		SELECT 'mm-dev-narrow', 'mm-dev-narrow@mm.test', 'Narrow', w.customer_id
		FROM core.application a JOIN core.workspace w ON w.id=a.workspace_id WHERE a.id=$1::uuid RETURNING id::text`, f.app1)
	q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		SELECT $1::uuid, 'developer', workspace_id FROM core.application WHERE id=$2::uuid RETURNING role::text`, narrow, f.app1)
	q(`INSERT INTO identity.user_model_access (user_id, model_id) VALUES ($1::uuid, $2::uuid) RETURNING model_id::text`, narrow, f.modelA)

	for _, tc := range []struct {
		what, sub, dim, app string
		want                int
	}{
		{"the developer, own workspace", "mm-dev", "Region A", f.app1, http.StatusOK},
		// A developer's reach on a builder route is its whole tenant
		// (roleReachesAppSQL's developer arm), so ws1b opens to mm-dev of ws1a.
		{"the developer, the tenant's other workspace", "mm-dev", "Region C", f.app2, http.StatusOK},
		{"the developer, another tenant", "mm-dev", "Region D", f.app3, http.StatusNotFound},
		{"a developer narrowed to model A, on model A", "mm-dev-narrow", "Region A", f.app1, http.StatusOK},
		{"a developer narrowed to model A, on model B", "mm-dev-narrow", "Region B", f.app1, http.StatusNotFound},
	} {
		dim, roots := dimOf(tc.dim)
		before := q(`SELECT string_agg(code||':'||sort_order, ',' ORDER BY code) FROM model.dimension_member WHERE dimension_id=$1::uuid`, dim)
		propCode, propRaw := f.do(t, "GET", "/api/developer/dimensions/"+dim+"/properties", tc.sub, tc.app, "", nil)
		code, raw := f.do(t, "PUT", "/api/developer/dimensions/"+dim+"/members/order", tc.sub, tc.app, "",
			map[string]any{"member_ids": strings.Split(roots, ",")})
		if code != tc.want {
			t.Errorf("%s: reorder %d %s, want %d", tc.what, code, raw, tc.want)
		}
		wantProps := map[int][]int{http.StatusOK: {http.StatusOK}, http.StatusNotFound: {http.StatusForbidden, http.StatusNotFound}}[tc.want]
		if propCode != wantProps[0] && (len(wantProps) < 2 || propCode != wantProps[1]) {
			t.Errorf("%s: the member edits' check answers %d %s where the reorder answers %d — the two checks drifted", tc.what, propCode, propRaw, code)
		}
		if tc.want != http.StatusOK {
			if after := q(`SELECT string_agg(code||':'||sort_order, ',' ORDER BY code) FROM model.dimension_member WHERE dimension_id=$1::uuid`, dim); after != before {
				t.Errorf("%s: a refused reorder changed %s to %s", tc.what, before, after)
			}
		}
	}
}

// Which side of the dimension a level's parent sits on follows the members,
// not the declared parent dimension: a developer can declare or clear
// parent_dimension_id after the members exist (PATCH …/dimensions/{id}), and
// every level they then have must stay reorderable.
func TestReorderDimensionMembers_ParentOnEitherSide(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	const dev = "mm-dev"
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	dim := func(name string, parentDim *string) string {
		return q(`INSERT INTO model.dimension_def (model_id, revision_id, name, parent_dimension_id)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid) RETURNING id::text`, f.modelA, f.revA, name, parentDim)
	}
	member := func(dimID, code string, parent *string, sortOrder int) string {
		return q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order)
			VALUES ($1::uuid, $2, $2, $3::uuid, $4) RETURNING id::text`, dimID, code, parent, sortOrder)
	}
	order := func(dimID string) string {
		return q(`SELECT string_agg(code, ',' ORDER BY sort_order, code) FROM model.dimension_member WHERE dimension_id=$1::uuid`, dimID)
	}
	call := func(method, path string, body any) {
		t.Helper()
		if code, raw := f.do(t, method, path, dev, f.app1, f.modelA, body); code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, code, raw)
		}
	}

	// Org's own hierarchy P1 {K1, K2}; then the developer declares Region A
	// its parent dimension. P1 is still K1's and K2's parent.
	region := q(`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='Region A'`, f.revA)
	org := dim("Org", nil)
	p1 := member(org, "P1", nil, 1)
	k1, k2 := member(org, "K1", &p1, 2), member(org, "K2", &p1, 3)
	call("PATCH", "/api/developer/dimensions/"+org, map[string]any{"parent_dimension_id": region})
	call("PUT", "/api/developer/dimensions/"+org+"/members/order", map[string]any{"parent_member_id": p1, "member_ids": []string{k2, k1}})
	if got := order(org); got != "P1,K2,K1" {
		t.Errorf("Org after reordering P1's children: %s, want P1,K2,K1", got)
	}
	// A member of the declared parent dimension with no children here is
	// still a level — an empty one.
	call("PUT", "/api/developer/dimensions/"+org+"/members/order", map[string]any{"parent_member_id": f.memberA, "member_ids": []string{}})

	// The reverse: Stores' members hang under T1 of Teams, its parent
	// dimension; then the developer clears the parent dimension. T1 is still
	// S1's and S2's parent.
	teams := dim("Teams", nil)
	t1 := member(teams, "T1", nil, 1)
	stores := dim("Stores", &teams)
	member(stores, "S0", nil, 1)
	s1, s2 := member(stores, "S1", &t1, 2), member(stores, "S2", &t1, 3)
	call("PATCH", "/api/developer/dimensions/"+stores, map[string]any{"parent_dimension_id": nil})
	if got := q(`SELECT COALESCE(parent_dimension_id::text, 'none') FROM model.dimension_def WHERE id=$1::uuid`, stores); got != "none" {
		t.Fatalf("Stores' parent dimension is %s after clearing it", got)
	}
	call("PUT", "/api/developer/dimensions/"+stores+"/members/order", map[string]any{"parent_member_id": t1, "member_ids": []string{s2, s1}})
	if got := order(stores); got != "S0,S2,S1" {
		t.Errorf("Stores after reordering T1's children: %s, want S0,S2,S1 (the group keeps its slots)", got)
	}
	if got := q(`SELECT metadata->>'parent_code' FROM audit.audit_event
		WHERE event_type='dimension.members_reordered' AND resource_id=$1 ORDER BY occurred_at DESC LIMIT 1`, stores); got != "T1" {
		t.Errorf("audit parent_code %q, want T1", got)
	}
}

// A member re-parented while a reorder runs is renumbered where it now
// sits: the reorder waits for the re-parent and reads the tree after it.
func TestReorderDimensionMembers_WaitsForReparent(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	// Org: R1 {C1, C2}, R2 {C3}, R3.
	dimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Org') RETURNING id::text`, f.modelA, f.revA)
	member := func(code string, parent *string, sortOrder int) string {
		return q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order)
			VALUES ($1::uuid, $2, $2, $3::uuid, $4) RETURNING id::text`, dimID, code, parent, sortOrder)
	}
	r1, r2, r3 := member("R1", nil, 1), member("R2", nil, 2), member("R3", nil, 3)
	c1 := member("C1", &r1, 4)
	member("C2", &r1, 5)
	member("C3", &r2, 6)

	// The member PATCH's statement, in a transaction left open: C1 moves
	// under R2.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE model.dimension_member SET parent_member_id=$2::uuid WHERE id=$1::uuid`, c1, r2); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := modeledit.ReorderMembers(ctx, f.pool, dimID, nil, []string{r3, r1, r2})
		done <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the reorder finished (%v) without waiting for the open re-parent", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the reorder never waited for the open re-parent")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reorder: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the reorder did not finish after the re-parent committed")
	}
	// Tree order of the tree after the re-parent: R2's children C1 (4) then C3 (6).
	if got := q(`SELECT string_agg(code, ',' ORDER BY sort_order, code) FROM model.dimension_member WHERE dimension_id=$1::uuid`, dimID); got != "R3,R1,C2,R2,C1,C3" {
		t.Errorf("stored order %s, want R3,R1,C2,R2,C1,C3 (C1 now under R2)", got)
	}
}
