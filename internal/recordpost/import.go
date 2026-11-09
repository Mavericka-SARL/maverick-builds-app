package recordpost

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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
	// A load that replaces (ReplaceBy) is whole or nothing either way.
	Partial bool
	// ReplaceBy names a field (ReplaceField): the table's records holding a
	// value of it that a row holds — a date field's month — are deleted in
	// the transaction that creates the rows' records, and what they posted
	// comes back out of the metrics. Every row must hold one.
	ReplaceBy string
	// DeleteRefusal says why CreatorID may not delete rec, a record the load
	// replaces ("" when they may); nil checks only what rec posted.
	DeleteRefusal func(rec *crudapp.FormRecord) string
	// ByPerson says CreatorID is the person loading the file, whom a refusal
	// addresses as "you".
	ByPerson bool
	// BeforeCommit, when set, runs in the records' transaction once every
	// record is created (res says how many, and how many were replaced),
	// just before it commits: what it writes commits with them, and its
	// error rolls the whole file back and is Import's. The records import
	// writes its Recent Imports line there, holding the Idempotency-Key the
	// file was sent under, so a file sent again is not created twice.
	BeforeCommit func(ctx context.Context, tx pgx.Tx, res ImportResult) error
}

// ImportResult is what an import did, or would do in a dry run.
type ImportResult struct {
	Created int
	// Replaced counts the records a ReplaceBy load deleted.
	Replaced int
	Errors   []RowError
}

// ImportRows is Import for a caller that does not replace.
func (ps *Poster) ImportRows(ctx context.Context, req ImportRequest) (created int, errs []RowError, err error) {
	res, err := ps.Import(ctx, req)
	return res.Created, res.Errors, err
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
func (ps *Poster) Import(ctx context.Context, req ImportRequest) (res ImportResult, err error) {
	form := req.Form
	var errs []RowError
	var replaceBy *crudapp.FormField
	if req.ReplaceBy != "" {
		if replaceBy, err = ReplaceField(form, req.ReplaceBy); err != nil {
			return res, err
		}
	}
	replaceKeys := map[string]bool{}
	partial := req.Partial && replaceBy == nil

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
	checks, err := ps.newTablePosting(ctx, form) // what a row posts, checked once per cell
	if err != nil {
		return res, err
	}
	if req.CreatorID != "" {
		if view, err = ps.AccessViewFor(ctx, req.CreatorID, form); err != nil {
			return res, err
		}
	}
	type stagedRecord struct {
		data   map[string]any
		status string
	}
	var staged []stagedRecord
	memberLeaves := NewMemberCache()
	today := time.Now().UTC().Format("2006-01-02")
	codeRows := map[string]int{} // typed code → the row that first gives it

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
		crudapp.ApplyDefaults(form, data, today)
		rowValid := true
		problems, pErr := ps.RecordValueProblems(ctx, view, form, nil, data, true, memberLeaves)
		if pErr != nil {
			return res, pErr
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
		if cf := form.TypedCodeField(); cf != nil && rowValid {
			if code, _ := data[cf.Name].(string); code != "" {
				if first, dup := codeRows[code]; dup {
					errs = append(errs, RowError{Row: row.RowNumber, Column: headerOf[cf.Name],
						Message: fmt.Sprintf("%s %q is also on row %d: a code is one record's", firstNonEmpty(cf.Label, cf.Name), code, first)})
					rowValid = false
				} else {
					codeRows[code] = row.RowNumber
				}
			}
		}
		if replaceBy != nil && rowValid {
			if key := replaceKey(replaceBy, data[replaceBy.Name]); key == "" {
				errs = append(errs, RowError{Row: row.RowNumber, Column: firstNonEmpty(headerOf[replaceBy.Name], replaceBy.Name),
					Message: fmt.Sprintf("this load replaces the records of each %s it holds, so every row needs one", firstNonEmpty(replaceBy.Label, replaceBy.Name))})
				rowValid = false
			} else {
				replaceKeys[key] = true
			}
		}
		if !rowValid {
			continue
		}
		// A row holding a value with a mapped field empty would post at a
		// partial coordinate: refused naming its column, whoever imports.
		if field, msg := checks.emptyMappedField("", &RecordState{Status: status, Data: data}); field != "" {
			errs = append(errs, RowError{Row: row.RowNumber, Column: firstNonEmpty(headerOf[field], field), Message: msg})
			continue
		}
		if req.CreatorID != "" {
			code, msg, rErr := ps.postingRefusal(ctx, checks, req.CreatorID, true, "", &RecordState{Status: status, Data: data})
			if rErr != nil {
				return res, rErr
			}
			if code != 0 {
				errs = append(errs, RowError{Row: row.RowNumber, Message: msg})
				continue
			}
		}
		staged = append(staged, stagedRecord{data: data, status: status})
	}

	res.Errors = errs
	if len(errs) > 0 && !partial {
		return res, nil
	}
	keys := make([]string, 0, len(replaceKeys))
	for k := range replaceKeys {
		keys = append(keys, k)
	}
	if req.DryRun {
		res.Created = len(staged)
		if replaceBy != nil && len(keys) > 0 {
			recs, rErr := recordsToReplace(ctx, ps.db(ctx), form, replaceBy, keys, false)
			if rErr != nil {
				return res, rErr
			}
			if msg, rErr := ps.replaceRefusal(ctx, req, checks, replaceBy, recs); rErr != nil {
				return res, rErr
			} else if msg != "" {
				return res, &ReplaceError{Msg: msg}
			}
			res.Replaced = len(recs)
		}
		return res, nil
	}

	membersAdded := false
	defer func() {
		if membersAdded && ps.MembersChanged != nil {
			ps.MembersChanged(context.WithoutCancel(ctx), form)
		}
	}()
	// Every row in one transaction: a file imports whole or not at all. Each
	// record used to commit on its own, so a request cut off part-way — a
	// client timeout, a closed tab — left the rows made so far (2,568 of a
	// 9,000-row GL file) with nothing saying which.
	tx, err := ps.db(ctx).Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	// The records the file replaces go first, in the same transaction: a
	// load that fails part-way leaves the month as it was.
	if replaceBy != nil && len(keys) > 0 {
		old, rErr := recordsToReplace(ctx, tx, form, replaceBy, keys, true)
		if rErr != nil {
			return res, rErr
		}
		if msg, rErr := ps.replaceRefusal(ctx, req, checks, replaceBy, old); rErr != nil {
			return res, rErr
		} else if msg != "" {
			return res, &ReplaceError{Msg: msg}
		}
		ids := make([]string, len(old))
		for i, r := range old {
			ids[i] = r.ID
		}
		if len(ids) > 0 {
			if _, dErr := tx.Exec(ctx, `DELETE FROM runtime.form_record WHERE id = ANY($1::uuid[])`, ids); dErr != nil {
				return res, fmt.Errorf("delete the records the load replaces: %w", dErr)
			}
		}
		res.Replaced = len(ids)
	}
	txStore := crudapp.NewStoreOn(tx)
	recs := make([]*crudapp.FormRecord, 0, len(staged))
	for _, sr := range staged {
		rec, cErr := txStore.CreateRecordThen(ctx, form.ID, req.CreatorID, sr.status, sr.data, ps.AfterRecordCreated(req.CreatorID, &membersAdded))
		if cErr != nil {
			membersAdded = false
			return ImportResult{}, fmt.Errorf("create record: %w", cErr)
		}
		recs = append(recs, rec)
	}
	if req.BeforeCommit != nil {
		done := res
		done.Created = len(recs)
		if err := req.BeforeCommit(ctx, tx, done); err != nil {
			membersAdded = false
			return ImportResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		membersAdded = false
		return ImportResult{}, fmt.Errorf("create records: %w", err)
	}
	res.Created = len(recs)
	creator := req.CreatorID
	// Replaced records posted through the table's mappings too: every total
	// is rebuilt, so a cell only an old record posted into empties.
	rebuildAll := res.Replaced > 0
	// Posted as one batch: each record's postings, then every total they
	// post into rebuilt once and one recalculation. Each record used to post
	// on its own goroutine, rebuilding its totals and recalculating
	// everything downstream per record — hundreds of overlapping passes for
	// one file (about nine records a second on an FP&A model).
	if req.Inline {
		ps.postBatch(ctx, form, recs, creator, rebuildAll)
	} else {
		bgCtx := context.WithoutCancel(ctx)
		go ps.postBatch(bgCtx, form, recs, creator, rebuildAll)
	}
	// The rules a record created by hand fires: an automation that starts a
	// workflow per submitted expense did not start for imported ones. The
	// records exist now whatever happens to the request.
	dispatchCtx := context.WithoutCancel(ctx)
	for _, rec := range recs {
		ps.DispatchRecordCreated(dispatchCtx, req.AppID, req.RevisionID, form.ID, rec.ID, rec.Status, creator, true)
	}
	return res, nil
}

// postBatch posts records created together through their table's live
// mappings — the table and its mappings read once, each cell checked once,
// one statement per mapping writing the postings — rebuilds each total they
// changed once and recalculates once. The records are posted as created,
// not read back: one changed since posts through its change instead
// (insertPostings).
func (ps *Poster) postBatch(ctx context.Context, form *crudapp.FormDef, recs []*crudapp.FormRecord, userID string, rebuildAll bool) {
	if len(recs) == 0 && !rebuildAll {
		return
	}
	defer ps.pending(ctx, form.ID)()
	tp, err := ps.newTablePosting(ctx, form)
	if err != nil {
		ps.Log.Error().Err(err).Str("form_id", form.ID).Msg("import: read mappings to post records")
		return
	}
	batch := make([]postingRecord, 0, len(recs))
	for _, rec := range recs {
		pr := postingRecord{id: rec.ID, status: rec.Status, data: asStored(rec.Data), updatedAt: rec.UpdatedAt}
		if rec.CreatedBy != nil {
			pr.creator = *rec.CreatedBy
		}
		batch = append(batch, pr)
	}
	touched := ps.post(ctx, tp, batch, userID)
	if rebuildAll {
		touched = touched[:0]
		for i := range tp.mappings {
			touched = append(touched, i)
		}
	}
	ps.totals(ctx, tp, touched, userID)
}
