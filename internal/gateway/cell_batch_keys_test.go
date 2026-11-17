package gateway

import (
	"strings"
	"testing"
)

// A batch cell naming no value, or a key a cell does not have, is refused
// naming it: {"date": …} used to be stored as 0 and answered "ok"
// (Territory & Quota build, 2026-11-16).
func TestCellBatchRefusesACellWithoutAValue(t *testing.T) {
	d := setupSalesDemo(t)
	for _, c := range []struct {
		cell map[string]any
		want string
	}{
		{map[string]any{"metric_id": d.metric["revenue"], "dim_codes": map[string]string{}, "date": "2027-01-01"}, `a cell has no \"date\"`},
		{map[string]any{"metric_id": d.metric["revenue"], "dim_codes": map[string]string{}}, "cell 0 names no value"},
	} {
		status, raw := d.req("POST", "/api/cells/batch", d.dev, map[string]any{"model_id": d.modelID, "revision_id": d.revID, "cells": []any{c.cell}})
		if status != 400 || !strings.Contains(string(raw), c.want) {
			t.Errorf("cell %v: %d %s, want 400 saying %q", c.cell, status, raw, c.want)
		}
	}
}
