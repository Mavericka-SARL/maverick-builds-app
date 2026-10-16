package modeledit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// MaxSelectorBarEntries caps a dashboard's selector bar: it is one row of
// selectors at the top of the page, not a filter panel.
const MaxSelectorBarEntries = 8

// SelectorBarEntry is one selector of a dashboard's selector bar
// (model.dashboard_def.selector_bar): the dimension it selects on and the
// member it opens on. Every grid and chart on the dashboard, and every KPI
// following the dashboard's selectors or the bar, follows the bar for that
// dimension and draws no selector of its own for it.
type SelectorBarEntry struct {
	DimensionID string `json:"dimension_id"`
	// Default is a member code of the dimension; empty opens on the
	// dimension's first member.
	Default string `json:"default,omitempty"`
}

// NormalizeSelectorBar checks a selector bar for a dashboard of modelID filed
// under revisionID ("" = filed under no revision) and returns it as stored.
// A dimension may be named by its id or by its name (the AI Developer names
// dimensions); either way it must belong to the dashboard's model and
// revision, may appear once, and Default must be one of its member codes.
// An empty bar is valid: the dashboard has none.
func NormalizeSelectorBar(ctx context.Context, db DB, modelID, revisionID string, entries []SelectorBarEntry) ([]SelectorBarEntry, error) {
	if len(entries) > MaxSelectorBarEntries {
		return nil, fmt.Errorf("selector_bar: at most %d dimensions", MaxSelectorBarEntries)
	}
	out := make([]SelectorBarEntry, 0, len(entries))
	seen := map[string]bool{}
	for i, e := range entries {
		ref := strings.TrimSpace(e.DimensionID)
		if ref == "" {
			return nil, fmt.Errorf("selector_bar[%d]: dimension_id is required", i)
		}
		var dimID, dimName string
		err := db.QueryRow(ctx, `
			SELECT id::text, name FROM model.dimension_def
			WHERE model_id = $1::uuid
			  AND (revision_id = NULLIF($2,'')::uuid OR revision_id IS NULL)
			  AND (id::text = $3 OR name = $3)
			ORDER BY (id::text = $3) DESC
			LIMIT 1`, modelID, revisionID, ref).Scan(&dimID, &dimName)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("selector_bar[%d]: dimension %q is not in this dashboard's revision", i, ref)
		}
		if err != nil {
			return nil, fmt.Errorf("selector_bar[%d]: %w", i, err)
		}
		if seen[dimID] {
			return nil, fmt.Errorf("selector_bar: dimension %q is listed twice", dimName)
		}
		seen[dimID] = true
		code := strings.TrimSpace(e.Default)
		if code != "" {
			var ok bool
			if err := db.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2)`,
				dimID, code).Scan(&ok); err != nil {
				return nil, fmt.Errorf("selector_bar[%d]: %w", i, err)
			}
			if !ok {
				return nil, fmt.Errorf("selector_bar: %q is not a member code of dimension %q", code, dimName)
			}
		}
		out = append(out, SelectorBarEntry{DimensionID: dimID, Default: code})
	}
	return out, nil
}

// SelectorBarJSON is the stored form of a normalized bar.
func SelectorBarJSON(entries []SelectorBarEntry) string {
	if len(entries) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(entries)
	return string(b)
}
