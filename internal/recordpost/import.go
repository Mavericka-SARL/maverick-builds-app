package recordpost

import (
	"context"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// RowError is one row an import refuses.
type RowError struct {
	Row     int    `json:"row"`
	Column  string `json:"column"`
	Message string `json:"message"`
}

// ImportRequest is rows to create as records of a table.
type ImportRequest struct {
	Form *crudapp.FormDef
	// CreatorID is the records' creator: the person importing, or the
	// principal a connector run runs as. Its access decides where the
	// records may post.
	CreatorID string
	Header    []string
	Rows      []importpkg.RawRow
	// NewStatus is the status a record starts in when its row names none: a
	// draft in a table with a status field, none otherwise.
	NewStatus string
	// StatusRefusal says why the creator may not create a record in status
	// (a row's status column, in a table with a status field); "" when they
	// may.
	StatusRefusal func(status string) string
	// AppID and RevisionID are where the automation rules a new record
	// fires are looked for.
	AppID, RevisionID string
	// DryRun checks every row and creates nothing.
	DryRun bool
	// Inline posts each record before ImportRows returns (a worker's run,
	// which has no request to answer first); otherwise each posts in the
	// background.
	Inline bool
	// Partial creates the rows that pass and returns the others' errors: a
	// connector's, so a scheduled sync does not stall on one bad upstream
	// record. Otherwise nothing is created when any row fails (a file).
	Partial bool
}

// ImportRows creates a record of req.Form for each row, posts it through the
// table's mappings and fires the rules a new record fires — the one path for
// a file import, a saved csv_import or google_sheets integration, a REST API
// connector and a model link whose target is a table. Validation is
// whole-file atomic unless req.Partial: when errs is not empty nothing was
// created. Each row is
// checked as a record saved by hand is: its values (RecordValueProblems) and
// what it would post (PostingRefusal). An auto-code column is read past: the
// server gives codes. created counts the records made, or that would be in a
// dry run.
func (ps *Poster) ImportRows(ctx context.Context, req ImportRequest) (created int, errs []RowError, err error) {
	form := req.Form
	store := crudapp.NewStore(ps.db(ctx))

	// Match each column header to a field by name, then by label
	// (case-insensitive) — how an exported file names its columns. Columns
	// matching neither (an export's own "id"/"created_at") are read past,
	// so a file exported and imported again just works.
	fieldByHeader := map[string]crudapp.FormField{}
	headerOf := map[string]string{} // field name → its column
	statusCol := ""
	for _, col := range req.Header {
		name := strings.TrimSpace(col)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		matched := false
		for _, f := range form.Fields {
			if strings.ToLower(f.Name) != lower && strings.ToLower(f.Label) != lower {
				continue
			}
			matched = true
			switch f.Type {
			case crudapp.FieldStatus:
				statusCol = col
			case crudapp.FieldAutoCode:
			default:
				fieldByHeader[col] = f
				headerOf[f.Name] = col
			}
			break
		}
		if !matched && lower == "status" && form.HasStatus() && statusCol == "" {
			statusCol = col
		}
	}

	// A dry run of a connector nobody has run yet has no principal: there
	// is nobody's access to check its rows against.
	view := &AccessView{}
	if req.CreatorID != "" {
		if view, err = ps.AccessViewFor(ctx, req.CreatorID, form); err != nil {
			return 0, nil, err
		}
	}
	type stagedRecord struct {
		data   map[string]any
		status string
	}
	var staged []stagedRecord
	memberLeaves := map[string]map[string]bool{} // dimension id → code → leaf

	for _, row := range req.Rows {
		data := map[string]any{}
		for col, field := range fieldByHeader {
			if raw := strings.TrimSpace(row.Cells[col]); raw != "" {
				data[field.Name] = raw
			}
		}
		statusRaw := ""
		if statusCol != "" {
			statusRaw = strings.ToLower(strings.TrimSpace(row.Cells[statusCol]))
		}
		if len(data) == 0 && statusRaw == "" {
			continue // blank row
		}
		rowValid := true
		problems, pErr := ps.RecordValueProblems(ctx, view, form, nil, data, true, memberLeaves)
		if pErr != nil {
			return 0, nil, pErr
		}
		for _, p := range problems {
			col := headerOf[p.Field]
			if col == "" {
				col = p.Field
			}
			errs = append(errs, RowError{Row: row.RowNumber, Column: col, Message: p.Message})
			rowValid = false
		}
		status := req.NewStatus
		if statusRaw != "" {
			if !crudapp.ValidRecordStatus(statusRaw) {
				errs = append(errs, RowError{Row: row.RowNumber, Column: statusCol, Message: fmt.Sprintf("%q is not a valid status (draft, submitted, approved, rejected)", statusRaw)})
				rowValid = false
			} else if msg := req.StatusRefusal(statusRaw); msg != "" {
				errs = append(errs, RowError{Row: row.RowNumber, Column: statusCol, Message: msg})
				rowValid = false
			} else {
				status = statusRaw
			}
		}
		if !rowValid {
			continue
		}
		if req.CreatorID != "" {
			code, msg, rErr := ps.PostingRefusal(ctx, req.CreatorID, true, form, "", &RecordState{Status: status, Data: data})
			if rErr != nil {
				return 0, nil, rErr
			}
			if code != 0 {
				errs = append(errs, RowError{Row: row.RowNumber, Message: msg})
				continue
			}
		}
		staged = append(staged, stagedRecord{data: data, status: status})
	}

	if len(errs) > 0 && !req.Partial {
		return 0, errs, nil
	}
	if req.DryRun {
		return len(staged), errs, nil
	}

	for _, sr := range staged {
		rec, cErr := store.CreateRecordWithStatus(ctx, form.ID, req.CreatorID, sr.status, sr.data)
		if cErr != nil {
			return created, nil, fmt.Errorf("create record: %w", cErr)
		}
		created++
		recID, status, data, creator := rec.ID, sr.status, sr.data, req.CreatorID
		if req.Inline {
			_ = ps.Apply(ctx, recID, form.ID, status, data, creator)
		} else {
			bgCtx := context.WithoutCancel(ctx)
			go func() { _ = ps.Apply(bgCtx, recID, form.ID, status, data, creator) }()
		}
		// The rules a record created by hand fires: an automation that
		// starts a workflow per submitted expense did not start for
		// imported ones.
		ps.DispatchRecordCreated(ctx, req.AppID, req.RevisionID, form.ID, recID, status, creator)
	}
	return created, errs, nil
}
