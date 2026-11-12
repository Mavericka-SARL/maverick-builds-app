package gateway

// The workbook map in the turn (aiassistant/workbook_map.go): its summary in
// every turn's instructions, its rows in the server's check after a confirmed
// plan, its metric names in the formulas read_attached_sheet shows, and the
// developer's view of it (GET /api/ai/sessions/{id}/workbook-map).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// maxMapBrief bounds the map's summary in a turn's instructions.
const maxMapBrief = 4000

// sessionWorkbooks are the workbooks attached to the session, kept whole,
// by file name.
func (h *handler) sessionWorkbooks(ctx context.Context, sessionID string) map[string][]byte {
	store := aiassistant.NewDocumentStore(h.db.For(ctx))
	docs, err := store.ListDocuments(ctx, sessionID)
	if err != nil {
		return nil
	}
	out := map[string][]byte{}
	for _, d := range docs {
		if !d.Importable {
			continue
		}
		low := strings.ToLower(d.Filename)
		if !strings.HasSuffix(low, ".xlsx") && !strings.HasSuffix(low, ".xlsm") {
			continue
		}
		if doc, raw, err := store.GetRaw(ctx, sessionID, d.Filename); err == nil && raw != nil {
			out[doc.Filename] = raw
		}
	}
	return out
}

// workbookMapBrief is the map's summary for a turn's instructions: one line
// per sheet compared so far, with its differing and not-yet-mapped rows.
func (h *handler) workbookMapBrief(ctx context.Context, sessionID, modelID, revID string) string {
	rows, err := aiassistant.LoadWorkbookMap(ctx, h.db.For(ctx), modelID, revID, nil)
	if err != nil || len(rows) == 0 {
		return ""
	}
	brief := aiassistant.RenderCoverage(aiassistant.Coverage(rows, h.sessionWorkbooks(ctx, sessionID)))
	if len(brief) > maxMapBrief {
		brief = brief[:maxMapBrief] + "\n(… more sheets: workbook_map lists them)\n"
	}
	return "\n\n" + brief
}

// mapSpecsForPlans are the map's rows the server's check after a confirmed
// plan compares again: every mapped row of each sheet a metric of the plans
// was mapped on, so a row the plan broke elsewhere on the sheet is found too.
// before gains each row's state when last compared.
func mapSpecsForPlans(rows []aiassistant.MapRow, touched map[string]bool, have []compareSpec, before map[string]map[int]rowResult) []compareSpec {
	sheets := map[string]bool{}
	for _, r := range rows {
		if touched[strings.ToLower(r.Metric)] {
			sheets[strings.ToLower(r.File+"\x00"+r.Sheet)] = true
		}
	}
	seen := map[string]bool{}
	for _, s := range have {
		seen[strings.ToLower(s.File+"\x00"+s.Sheet+"\x00"+fmt.Sprint(s.Row)+"\x00"+membersKey(s.Members))] = true
	}
	var out []compareSpec
	for _, r := range rows {
		if !sheets[strings.ToLower(r.File+"\x00"+r.Sheet)] {
			continue
		}
		key := strings.ToLower(r.File + "\x00" + r.Sheet + "\x00" + fmt.Sprint(r.Row) + "\x00" + membersKey(r.Members))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, compareSpec{File: r.File, Sheet: r.Sheet, Row: r.Row, FirstColumn: r.FirstColumn, Metric: r.Metric, Members: r.Members})
		if r.Compared > 0 {
			sk := strings.ToLower(r.Sheet)
			if before[sk] == nil {
				before[sk] = map[int]rowResult{}
			}
			if _, known := before[sk][r.Row]; !known {
				before[sk][r.Row] = rowResult{r.Matched, r.Compared}
			}
		}
	}
	return out
}

// planMetricNames are the metrics plans create or change, lower-cased.
func planMetricNames(props []aiassistant.Proposal) map[string]bool {
	out := map[string]bool{}
	for _, p := range props {
		for _, st := range p.Steps {
			if st.Tool != "create_metric" && st.Tool != "update_metric" {
				continue
			}
			var sp struct {
				Name     string `json:"name"`
				MetricID string `json:"metric_id"`
			}
			if json.Unmarshal(st.Params, &sp) == nil {
				for _, n := range []string{sp.Name, sp.MetricID} {
					if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
						out[n] = true
					}
				}
			}
		}
	}
	return out
}

// aiWorkbookMap answers GET /api/ai/sessions/{id}/workbook-map: the map of
// the session's model, by sheet, for the developer.
func (h *handler) aiWorkbookMap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/"), "/")
	sess, err := h.aiChatStore(ctx).GetSession(ctx, parts[0])
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	rows, err := aiassistant.LoadWorkbookMap(ctx, h.db.For(ctx), sess.ModelID, sess.DraftRevisionID, nil)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	type sheetView struct {
		File      string         `json:"file"`
		Sheet     string         `json:"sheet"`
		Mapped    int            `json:"mapped"`
		Match     int            `json:"match"`
		Differ    []int          `json:"differ"`
		NotMapped []int          `json:"not_mapped"`
		Labels    map[int]string `json:"not_mapped_labels,omitempty"`
	}
	sheets := []sheetView{}
	for _, c := range aiassistant.Coverage(rows, h.sessionWorkbooks(ctx, sess.ID)) {
		sheets = append(sheets, sheetView{File: c.File, Sheet: c.Sheet, Mapped: c.Mapped, Match: c.Match,
			Differ: nonNilInts(c.Differ), NotMapped: nonNilInts(c.NotMapped), Labels: c.NotMappedLabels})
	}
	if rows == nil {
		rows = []aiassistant.MapRow{}
	}
	jsonOK(w, map[string]any{"sheets": sheets, "rows": rows})
}

func nonNilInts(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}
