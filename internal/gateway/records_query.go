package gateway

// The records query (POST /api/forms/{id}/records/query): a page of a
// table's records matching filters, a search and a sort — the records
// widget's list — or totals over them grouped by fields — what the chat
// connector and the AI assistant answer "how many", "how much" and "by
// what" with. It reads, as the caller, exactly the records the record list
// shows them (recordVisibility), and changes nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// recordsQueryResponse is the records query's answer: the table's columns
// with a page of records and how many match in all, or the totals' columns
// and rows with how many groups there are.
type recordsQueryResponse struct {
	Kind    string            `json:"kind"` // "records" or "totals"
	Table   recordsQueryTable `json:"table"`
	Columns any               `json:"columns"`
	// A records answer.
	Records []*crudapp.FormRecord `json:"records,omitempty"`
	Total   *int                  `json:"total,omitempty"`
	// A totals answer.
	Rows        []map[string]any `json:"rows,omitempty"`
	TotalGroups *int             `json:"total_groups,omitempty"`
	// The query answered, the page bounds, and the statuses a record of
	// the table may have (none without a status field).
	Query    crudapp.Query  `json:"query"`
	Limits   map[string]int `json:"limits"`
	Statuses []string       `json:"statuses,omitempty"`
}

type recordsQueryTable struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label"`
}

func (h *handler) recordsQuery(w http.ResponseWriter, r *http.Request, formID string) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	scope, err := h.resolveFormRecordScope(ctx, act, formID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !scope.reach {
		jsonRecordErr(w, errRecordNotReached, "table")
		return
	}
	var q crudapp.Query
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil && !errors.Is(err, io.EOF) {
		jsonErr(w, fmt.Errorf("invalid query: %v (it takes filters, search, sort, group_by, aggregates, limit and offset)", err), http.StatusBadRequest)
		return
	}
	vis, err := h.recordVisibility(ctx, scope)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	store := crudapp.NewStore(h.db.For(ctx))
	resp := recordsQueryResponse{
		Table:  recordsQueryTable{ID: scope.form.ID, Name: scope.form.Name, Label: scope.form.Label},
		Query:  q,
		Limits: map[string]int{"records": crudapp.MaxRecordLimit, "groups": crudapp.MaxGroupLimit},
	}
	if scope.hasStatus() {
		resp.Statuses = crudapp.RecordStatuses
	}
	if q.IsTotals() {
		res, err := store.QueryTotals(ctx, scope.form, vis, act.UserID, q)
		if crudapp.IsQueryError(err) {
			jsonErr(w, err, http.StatusBadRequest)
			return
		} else if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		resp.Kind, resp.Columns, resp.Rows, resp.TotalGroups = "totals", res.Columns, res.Rows, &res.TotalGroups
		jsonOK(w, resp)
		return
	}
	res, err := store.QueryRecords(ctx, scope.form, vis, act.UserID, q)
	if crudapp.IsQueryError(err) {
		jsonErr(w, err, http.StatusBadRequest)
		return
	} else if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	for _, rec := range res.Records {
		scope.permit(rec)
	}
	resp.Kind, resp.Columns, resp.Records, resp.Total = "records", scope.form.Columns(), res.Records, &res.Total
	jsonOK(w, resp)
}

// checkRecordSave checks a save, by the scope's caller, of record rec of
// the scope's table (nil for a record being created) that leaves it as
// `to`: its values, as the caller may see them (field problems, 400), then
// what it will post, which must be within its creator's access
// (postingRefusal: a status code and message). required checks every
// required field has a value.
func (h *handler) checkRecordSave(ctx context.Context, scope formRecordScope, rec *crudapp.FormRecord, to *recordState, required bool) (int, string, []fieldProblem, error) {
	view, err := h.accessViewFor(ctx, scope.userID, scope.form)
	if err != nil {
		return 0, "", nil, err
	}
	var current map[string]any
	recordID, creator := "", scope.userID
	if rec != nil {
		current, recordID = rec.Data, rec.ID
		// A legacy record with no creator posts within the access of
		// whoever changes it.
		if rec.CreatedBy != nil {
			creator = *rec.CreatedBy
		}
	}
	problems, err := h.recordValueProblems(ctx, view, scope.form, current, to.Data, required, newMemberCache())
	if err != nil || len(problems) > 0 {
		return 0, "", problems, err
	}
	// In a table everyone edits, a member read-only for the caller is one
	// they see records of but do not put a record in.
	if field := scope.readOnlyMember(to.Data); field != "" {
		return http.StatusForbidden, fmt.Sprintf("forbidden: the %s of this record is read-only for you", field), nil, nil
	}
	code, msg, err := h.postingRefusal(ctx, creator, creator == scope.userID, scope.form, recordID, to)
	return code, msg, nil, err
}

// recordQueryParam reads an export's optional ?query= (the records query's
// JSON), so an export holds what the widget shows.
func recordQueryParam(r *http.Request) (crudapp.Query, error) {
	var q crudapp.Query
	raw := strings.TrimSpace(r.URL.Query().Get("query"))
	if raw == "" {
		return q, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil {
		return q, fmt.Errorf("invalid query: %v", err)
	}
	if q.IsTotals() {
		return q, fmt.Errorf("an export lists records: its query has no group_by or aggregates")
	}
	return q, nil
}
