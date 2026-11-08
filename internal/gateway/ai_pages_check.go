package gateway

// Pages called done that leave sheets of the workbook without one (2026-11-08:
// the AI-built production model had 6 pages where the hand-built one has 15;
// Demand, Capacity, Production, Logistics, Revenue, Manufacturing and
// Variance had none). A reply calling the pages done is sent back once with
// the attached workbook's sheets that no page shows: no page named after the
// sheet, and no grid of its name on a page.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var pagesDoneClaim = regexp.MustCompile(`(?i)\b(pages?|dashboards?)\b[^.!?\n]{0,80}\b(done|complete[ds]?|finished|ready|in place|built)\b|\ball (?:the )?(pages|dashboards)\b`)

// notAPage: sheets people do not read as a page — import sheets, a guide,
// contents, lists, mappings.
var notAPage = regexp.MustCompile(`(?i)^(import|guide|contents|readme|read me|lists?|mapping|lookup|notes?)\b`)

// sheetKey is a sheet, page or grid name compared: letters and digits.
func sheetKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// pagesDoneNudge is the send-back for a reply calling the pages done while
// sheets of the attached workbook have none ("" when it does not, or all do).
func (h *handler) pagesDoneNudge(ctx context.Context, sessionID, revisionID, reply string) string {
	if revisionID == "" || !pagesDoneClaim.MatchString(reply) {
		return ""
	}
	sheets := h.attachedSheetNames(ctx, sessionID)
	if len(sheets) == 0 {
		return ""
	}
	rows, err := h.db.For(ctx).Query(ctx, `
		SELECT d.name, COALESCE(g.name, '')
		FROM model.dashboard_def d
		LEFT JOIN model.dashboard_widget w ON w.dashboard_id = d.id AND w.widget_type = 'grid'
		LEFT JOIN model.grid_def g ON g.id::text = w.ref_id
		WHERE d.revision_id = $1::uuid`, revisionID)
	if err != nil {
		return ""
	}
	var shown []string
	for rows.Next() {
		var page, grid string
		if rows.Scan(&page, &grid) == nil {
			shown = append(shown, sheetKey(page), sheetKey(grid))
		}
	}
	rows.Close()
	var missing []string
	for _, sheet := range sheets {
		key := sheetKey(sheet)
		if key == "" || notAPage.MatchString(sheet) {
			continue
		}
		covered := false
		for _, s := range shown {
			if s != "" && (s == key || strings.Contains(s, key) || (len(s) >= 4 && strings.Contains(key, s))) {
				covered = true
				break
			}
		}
		if !covered {
			missing = append(missing, sheet)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("Before calling the pages done: the workbook's sheets %s have no page showing them (no page named after them, no grid of their name on a page). People read every sheet the workbook has: give each a page, or a place on a page with the sheets it belongs with (inputs with their owners; plans; reports), given to the roles that use it — or say in your reply why a sheet needs none.",
		strings.Join(missing, ", "))
}
