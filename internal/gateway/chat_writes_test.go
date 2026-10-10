package gateway

// Writing cells from a chat connection (write_cells → POST /api/cells/batch):
// as the person, under the console's own checks, all or nothing, only for a
// token that carries the write scope, and only while the tenant allows it.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestChatWritesCells(t *testing.T) {
	f := setupConnector(t)
	ctx := context.Background()
	// Wes may look at the US but not change it (a "read" rule on the leaf:
	// an ancestor's "read" does not reach its children — writeguard).
	f.setAccessRules(f.westRepID, member(f.geo["US"], "read"))
	west := f.as(f.westRep)

	leaf := func(geo, prod, period string) map[string]string {
		return map[string]string{f.geoDim: geo, f.prodDim: prod, f.periodDim: period}
	}
	cell := func(metric string, at map[string]string, v float64) map[string]any {
		return map[string]any{"metric_id": f.metric[metric], "members": at, "value": v}
	}
	write := func(c *mcpClient, dryRun bool, cells ...map[string]any) (map[string]any, string) {
		t.Helper()
		return c.call("write_cells", f.ctxArgs(map[string]any{"cells": cells, "dry_run": dryRun}))
	}
	total := func(metric string, at map[string]string) float64 {
		t.Helper()
		return f.restTotals(f.dev, at)[f.metric[metric]]
	}
	ukLaptopQ1 := leaf("UK", "LAPTOP", "Q1")

	t.Run("the connection says it may write", func(t *testing.T) {
		a := west.must("get_connection_access", nil)
		if a["read_only"] != false {
			t.Errorf("read_only = %v, want false for a token with the write scope", a["read_only"])
		}
	})

	t.Run("a dry run checks the cells and writes none", func(t *testing.T) {
		res, errText := write(west, true, cell("revenue", ukLaptopQ1, 950_000))
		if errText != "" || res["status"] != "valid" || res["cells"] != float64(1) {
			t.Fatalf("dry run: %v %q", res, errText)
		}
		if got := total("revenue", ukLaptopQ1); !nearly(got, 900_000) {
			t.Errorf("revenue after a dry run = %v, want the seeded 900000", got)
		}
	})

	t.Run("one refused cell refuses the whole write", func(t *testing.T) {
		_, errText := write(west, false,
			cell("revenue", ukLaptopQ1, 950_000),
			cell("revenue", leaf("US", "LICENSE", "Q1"), 1),
			cell("margin", ukLaptopQ1, 1))
		if !strings.HasPrefix(errText, "refused: nothing was written") ||
			!strings.Contains(errText, "cell 1 ") || !strings.Contains(errText, "cell 2 ") || strings.Contains(errText, "cell 0 ") {
			t.Errorf("refusal = %q, want cells 1 (a read-only member) and 2 (a calculated metric) named, nothing written", errText)
		}
		if got := total("revenue", ukLaptopQ1); !nearly(got, 900_000) {
			t.Errorf("revenue after a refused write = %v, want 900000 — the allowed cell must not land alone", got)
		}
	})

	t.Run("a write lands every cell and recalculates", func(t *testing.T) {
		res, errText := write(west, false,
			cell("revenue", ukLaptopQ1, 950_000),
			cell("cost", ukLaptopQ1, 650_000),
			cell("target", leaf("DE", "LAPTOP", "Q2"), 123))
		if errText != "" || res["status"] != "written" || res["cells"] != float64(3) {
			t.Fatalf("write: %v %q", res, errText)
		}
		if got := total("revenue", ukLaptopQ1); !nearly(got, 950_000) {
			t.Errorf("revenue = %v, want 950000", got)
		}
		// The write answers after the recalculation: margin is new at once.
		if got, ok := f.calcValue("margin", ukLaptopQ1); !ok || !nearly(got, 300_000) {
			t.Errorf("margin = %v (%v), want 950000 - 650000", got, ok)
		}
		read := west.must("query_grid", f.ctxArgs(map[string]any{"grid_id": f.gridID, "metric_ids": []string{f.metric["revenue"]},
			"filters": map[string]string{f.geoDim: "UK", f.prodDim: "LAPTOP", f.periodDim: "Q1"}}))
		if v, _ := valueOf(t, read["rows"].([]any)[0].(map[string]any)["value"]); !nearly(v, 950_000) {
			t.Errorf("query_grid after the write = %v, want 950000", v)
		}
	})

	t.Run("the same cell twice is refused", func(t *testing.T) {
		_, errText := write(west, false, cell("units", ukLaptopQ1, 1), cell("units", ukLaptopQ1, 2))
		if !strings.Contains(errText, "the same cell as cell 0") {
			t.Errorf("duplicate cell: %q", errText)
		}
	})

	t.Run("a clear empties the cell", func(t *testing.T) {
		res, errText := write(west, false, map[string]any{"metric_id": f.metric["target"], "members": leaf("DE", "LAPTOP", "Q2"), "clear": true})
		if errText != "" || res["cleared"] != float64(1) {
			t.Fatalf("clear: %v %q", res, errText)
		}
		var n int
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE metric_id=$1::uuid AND dim_members=$2::jsonb`,
			f.metric["target"], mustJSON(t, leaf("DE", "LAPTOP", "Q2"))).Scan(&n)
		if n != 0 {
			t.Errorf("%d rows left in a cleared cell", n)
		}
		if _, errText := write(west, false, map[string]any{"metric_id": f.metric["target"], "members": ukLaptopQ1, "clear": true, "value": 1}); !strings.Contains(errText, "exactly one of") {
			t.Errorf("a cell with a value and a clear: %q", errText)
		}
	})

	t.Run("the write is the person's, made through the chat", func(t *testing.T) {
		var by string
		_ = f.pool.QueryRow(ctx, `SELECT entered_by::text FROM runtime.fact_input WHERE metric_id=$1::uuid AND dim_members=$2::jsonb ORDER BY entered_at DESC LIMIT 1`,
			f.metric["revenue"], mustJSON(t, ukLaptopQ1)).Scan(&by)
		if by != f.westRepID {
			t.Errorf("entered_by = %s, want Wes (%s)", by, f.westRepID)
		}
		var actor, via string
		if err := f.pool.QueryRow(ctx, `SELECT actor_user_id::text, metadata->>'via' FROM audit.audit_event WHERE event_type='cells.written' ORDER BY occurred_at DESC LIMIT 1`).Scan(&actor, &via); err != nil {
			t.Fatalf("no cells.written audit event: %v", err)
		}
		if actor != f.westRepID || via != "dev" {
			t.Errorf("audit: actor %s via %q, want Wes via the dev client", actor, via)
		}
	})

	t.Run("a connection granted reading only writes nothing", func(t *testing.T) {
		ro := connectMCP(t, f.url, "dev-read:"+f.westRep)
		if _, errText := write(ro, false, cell("units", ukLaptopQ1, 5)); !strings.HasPrefix(errText, "read_only:") {
			t.Errorf("write without the write scope: %q", errText)
		}
		if a := ro.must("get_connection_access", nil); a["read_only"] != true {
			t.Errorf("read_only = %v for a read-scope token", a["read_only"])
		}
	})

	t.Run("a tenant administrator turns chat writes off, and on", func(t *testing.T) {
		var adminID string
		if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('demo-tenant-admin', 'ta@demo.co', 'Tia Admin', $1::uuid) RETURNING id::text`, f.custID).Scan(&adminID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, adminID, f.wsID); err != nil {
			t.Fatal(err)
		}
		set := func(persona string, on bool) (int, map[string]any) {
			status, raw := f.req("PUT", "/api/admin/connector-settings", persona, map[string]any{"chat_writes": on})
			var out map[string]any
			_ = json.Unmarshal(raw, &out)
			return status, out
		}
		if status, _ := set(f.westRep, false); status != http.StatusForbidden {
			t.Errorf("a business user switching chat writes: %d, want 403", status)
		}
		if status, out := set("demo-tenant-admin", false); status != http.StatusOK || out["chat_writes"] != false {
			t.Fatalf("tenant admin switches off: %d %v", status, out)
		}
		if _, errText := write(west, false, cell("units", ukLaptopQ1, 5)); !strings.Contains(errText, "turned off changing data from chat") {
			t.Errorf("a chat write while switched off: %q", errText)
		}
		west.must("query_grid", f.ctxArgs(map[string]any{"grid_id": f.gridID}))
		status, raw := f.req("POST", "/api/cells/batch", f.westRep, map[string]any{"model_id": f.modelID,
			"cells": []map[string]any{{"metric_id": f.metric["units"], "dim_codes": ukLaptopQ1, "value": 5}}})
		if status != http.StatusOK {
			t.Errorf("the console's batch write while chat writes are off: %d %s", status, raw)
		}
		if status, out := set("demo-tenant-admin", true); status != http.StatusOK || out["chat_writes"] != true {
			t.Fatalf("tenant admin switches on: %d %v", status, out)
		}
		if _, errText := write(west, false, cell("units", ukLaptopQ1, 6)); errText != "" {
			t.Errorf("a chat write switched back on: %q", errText)
		}
		status, raw = f.req("GET", "/api/admin/connector-settings", "demo-tenant-admin", nil)
		if status != http.StatusOK || !strings.Contains(string(raw), `"chat_writes":true`) || !strings.Contains(string(raw), f.custID) {
			t.Errorf("settings: %d %s", status, raw)
		}
	})
}

func TestCellsBatchRoute(t *testing.T) {
	f := setupConnector(t)
	leaf := map[string]string{f.geoDim: "UK", f.prodDim: "LAPTOP", f.periodDim: "Q1"}
	batch := func(persona string, body map[string]any) (int, string) {
		body["model_id"] = f.modelID
		status, raw := f.req("POST", "/api/cells/batch", persona, body)
		return status, string(raw)
	}
	one := func(metric string, v float64) map[string]any {
		return map[string]any{"metric_id": f.metric[metric], "dim_codes": leaf, "value": v}
	}

	if status, raw := batch(f.westRep, map[string]any{"cells": []map[string]any{}}); status != http.StatusBadRequest {
		t.Errorf("an empty batch: %d %s", status, raw)
	}
	many := make([]map[string]any, maxCellBatch+1)
	for i := range many {
		many[i] = one("units", float64(i))
	}
	if status, raw := batch(f.westRep, map[string]any{"cells": many}); status != http.StatusBadRequest || !strings.Contains(raw, "1 to 500") {
		t.Errorf("an oversized batch: %d %s", status, raw)
	}
	status, raw := batch(f.westRep, map[string]any{"cells": []map[string]any{one("units", 7), one("margin", 1)}})
	if status != http.StatusUnprocessableEntity || !strings.Contains(raw, `"index":1`) || !strings.Contains(raw, "metric is not writable") {
		t.Errorf("a batch with a calculated metric: %d %s", status, raw)
	}
	if got := f.restTotals(f.dev, leaf)[f.metric["units"]]; !nearly(got, 900) {
		t.Errorf("units after a refused batch = %v, want the seeded 900", got)
	}
	status, raw = batch(f.westRep, map[string]any{"cells": []map[string]any{one("units", 7), one("revenue", 7000)}})
	if status != http.StatusOK || !strings.Contains(raw, `"cells":2`) {
		t.Fatalf("a batch: %d %s", status, raw)
	}
	if got := f.restTotals(f.dev, leaf)[f.metric["units"]]; !nearly(got, 7) {
		t.Errorf("units after the batch = %v, want 7", got)
	}
	status, raw = batch(f.ro, map[string]any{"revision_id": "00000000-0000-0000-0000-000000000000", "cells": []map[string]any{one("units", 1)}})
	if status != http.StatusForbidden {
		t.Errorf("a revision outside the model: %d %s", status, raw)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
