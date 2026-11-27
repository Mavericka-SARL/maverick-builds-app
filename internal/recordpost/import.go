package recordpost

import (
	"context"
	"encoding/json"
	"errors"
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
	// Admin: the creator administers the table's application, so the rows
	// may make the moves a field's transitions keep for some roles.
	Admin  bool
	Header []string
	Rows   []importpkg.RawRow
	// NewStatus is the status a record starts in when its row names none: a
	// draft in a table with a status field, none otherwise.
	NewStatus string
	// StatusRefusal says why the creator may not create a record in status
	// (a row's status column, in a table with a status field); "" when they
	// may.
	StatusRefusal func(status string) string
	// RecordRefusal, when set, says why a row's record may not be created
	// as the creator ("" when it may): a row naming a record its table
	// belongs to that is approved or rejected, for someone who is not an
	// administrator.
	RecordRefusal func(data map[string]any) string
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
	// UpdateRefusal says why CreatorID may not change rec, the record
	// holding a row's code in a table whose records have a typed code, to
	// status ("" when they may): the row then updates that record instead
	// of adding one. Nil refuses every such row (the code is taken).
	UpdateRefusal func(rec *crudapp.FormRecord, status string) string
	// MatchBy names a field (MatchField) a row is matched to a record by: a
	// row whose value of it one record holds updates that record, as a row
	// holding a typed code does (UpdateRefusal decides who may); a row
	// whose value no record holds adds a record — refused for an auto-code,
	// which only the server gives. "" matches by the table's typed code,
	// if it has one. A load does not both match and replace (ReplaceBy).
	MatchBy string
	// MissingKeeps: a row with no cell for a column keeps the value the
	// record it updates holds — an AI save_records record leaving a field
	// out, in a step whose columns are every field any record gives. Off, a
	// missing cell is an empty one: a spreadsheet row drops its trailing
	// blank cells.
	MissingKeeps bool
	// VisibleIDs, when set, keeps the ids of the records CreatorID may see:
	// a row is matched only to one of those, as if the others were not
	// there. Nil: every record (a connector's principal).
	VisibleIDs func(ctx context.Context, ids []string) (map[string]bool, error)
	// NewMembers, when set, is told how many records the file adds before
	// anything is written (a table keeping a dimension: each is a new
	// member); its error refuses the file.
	NewMembers func(n int) error
}

// ImportResult is what an import did, or would do in a dry run.
type ImportResult struct {
	Created int
	// Updated counts the records a row changed: a row whose typed code a
	// record already holds updates that record (a CRM extract loaded
	// again). Unchanged counts those the row left as they were.
	Updated   int
	Unchanged int
	// Replaced counts the records a ReplaceBy load deleted.
	Replaced int
	Errors   []RowError
}

// Written is how many records the import created or changed.
func (r ImportResult) Written() int { return r.Created + r.Updated }

// ImportRows is Import for a caller that does not replace: created counts
// the records it added or updated.
func (ps *Poster) ImportRows(ctx context.Context, req ImportRequest) (created int, errs []RowError, err error) {
	res, err := ps.Import(ctx, req)
	return res.Written(), res.Errors, err
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
	var replaceBy replaceSet
	if strings.TrimSpace(req.ReplaceBy) != "" {
		if replaceBy, err = ReplaceFields(form, req.ReplaceBy); err != nil {
			return res, err
		}
	}
	replaceKeys := map[string]bool{}
	partial := req.Partial && replaceBy == nil
	// The field a row is matched to a record by: the one the load names,
	// else the table's typed code.
	matchField := form.TypedCodeField()
	if strings.TrimSpace(req.MatchBy) != "" {
		if replaceBy != nil {
			return res, fmt.Errorf("match_by: a load either replaces records (replace_by) or updates them by a field (match_by), not both")
		}
		if matchField, err = MatchField(form, req.MatchBy); err != nil {
			return res, err
		}
	}

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
				headerOf[f.Name] = col // read only to match a row to its record
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
		view.SetAdmin(req.Admin)
	}
	type stagedRecord struct {
		data   map[string]any
		status string
		// target is the record the row updates: the one holding its typed
		// code. Nil adds a record.
		target *crudapp.FormRecord
		// unchanged: the row holds what target holds. Staged only in a load
		// that replaces (replace_by): if target goes with its period the row
		// adds it anew, else it is left as it is.
		unchanged bool
	}
	var staged []stagedRecord
	memberLeaves := NewMemberCache()
	today := time.Now().UTC().Format("2006-01-02")
	matchRows := map[string]int{} // match value → the row that first gives it
	// A row giving the value a record holds of the match field updates
	// that record — a CRM extract or an HRIS roster loaded again refreshes
	// its accounts and sellers. Each such load used to be refused whole
	// ("is taken") although a kept dimension's records are documented as
	// added and updated by their code (2026-11-16).
	// A file naming values for the table's auto code in a plain load (no
	// replace_by, no match_by) does what the table says (existing_codes,
	// migration 193): "update" updates the records holding them, as
	// match_by would; "refuse" refuses each such row — loading a file twice
	// used to add every record twice. A table made before it ("") ignores
	// the values, as it always did.
	var refuseCodes *crudapp.FormField
	if auto := form.CodeField(); auto != nil && auto.Type == crudapp.FieldAutoCode && matchField == nil && replaceBy == nil &&
		strings.TrimSpace(req.MatchBy) == "" && headerOf[auto.Name] != "" {
		switch form.ExistingCodes {
		case crudapp.CodesUpdate:
			matchField = auto
		case crudapp.CodesRefuse:
			refuseCodes = auto
		}
	}
	var matches *recordMatches
	if matchField != nil {
		if matches, err = recordsByMatch(ctx, ps.db(ctx), form.ID, matchField); err != nil {
			return res, err
		}
		keys := make([]string, 0, len(req.Rows))
		for _, row := range req.Rows {
			keys = append(keys, matchKey(matchField, row.Cells[headerOf[matchField.Name]]))
		}
		if err := matches.keepVisible(ctx, keys, req.VisibleIDs); err != nil {
			return res, err
		}
	}

	for _, row := range req.Rows {
		var target *crudapp.FormRecord
		matchVal := ""
		if matchField != nil {
			matchCol := firstNonEmpty(headerOf[matchField.Name], firstNonEmpty(matchField.Label, matchField.Name))
			matchVal = matchKey(matchField, row.Cells[headerOf[matchField.Name]])
			t, n := matches.lookup(matchVal)
			switch {
			case n > 1:
				errs = append(errs, RowError{Row: row.RowNumber, Column: matchCol,
					Message: fmt.Sprintf("%d records of %s hold %s %q, so the row cannot say which one it updates: delete the extra records first",
						n, form.Label, firstNonEmpty(matchField.Label, matchField.Name), matchVal)})
				continue
			case t == nil && matchVal != "" && matchField.Type == crudapp.FieldAutoCode:
				errs = append(errs, RowError{Row: row.RowNumber, Column: matchCol,
					Message: fmt.Sprintf("no record of %s has %s %q: the server numbers a new record, so leave its %s empty to add one",
						form.Label, firstNonEmpty(matchField.Label, matchField.Name), matchVal, firstNonEmpty(matchField.Label, matchField.Name))})
				continue
			}
			target = t
		}
		if refuseCodes != nil {
			if code := strings.TrimSpace(row.Cells[headerOf[refuseCodes.Name]]); code != "" {
				label := firstNonEmpty(refuseCodes.Label, refuseCodes.Name)
				errs = append(errs, RowError{Row: row.RowNumber, Column: headerOf[refuseCodes.Name], Message: fmt.Sprintf(
					"%s %q: %s numbers its new records itself, so leave %s empty to add a record (this table refuses a file row naming one; its developer can make such rows update their records instead)",
					label, code, form.Label, label)})
				continue
			}
		}
		data := map[string]any{}
		if target != nil {
			// The file's columns replace the record's values (an empty cell
			// empties its field); a field the file has no column for keeps
			// its value. A cell holding what the record holds keeps the
			// stored value, typed as stored.
			for k, v := range target.Data {
				data[k] = v
			}
			for col, field := range fieldByHeader {
				cell, given := row.Cells[col]
				if !given && req.MissingKeeps {
					continue
				}
				raw := strings.TrimSpace(cell)
				old, had := data[field.Name]
				switch {
				case raw == "":
					delete(data, field.Name)
				case had && sameValue(old, raw):
				default:
					data[field.Name] = raw
				}
			}
			data = crudapp.KeepServerValues(form, target.Data, data)
		} else {
			for col, field := range fieldByHeader {
				if raw := strings.TrimSpace(row.Cells[col]); raw != "" {
					data[field.Name] = raw
				}
			}
		}
		statusRaw := ""
		if statusCol != "" {
			statusRaw = strings.ToLower(strings.TrimSpace(row.Cells[statusCol]))
		}
		if target == nil && len(data) == 0 && statusRaw == "" {
			continue // blank row
		}
		var current map[string]any
		if target != nil {
			current = target.Data
		} else {
			crudapp.ApplyDefaults(form, data, today)
		}
		rowValid := true
		problems, pErr := ps.RecordValueProblems(ctx, view, form, current, data, true, memberLeaves)
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
		if target != nil {
			status = target.Status // a row naming no status keeps the record's
		}
		if statusRaw != "" {
			if !crudapp.ValidRecordStatus(statusRaw) {
				errs = append(errs, RowError{Row: row.RowNumber, Column: statusCol, Message: fmt.Sprintf("%q is not a valid status (draft, submitted, approved, rejected)", statusRaw)})
				rowValid = false
			} else if target != nil {
				status = statusRaw // UpdateRefusal checks the move below
			} else if msg := req.StatusRefusal(statusRaw); msg != "" {
				errs = append(errs, RowError{Row: row.RowNumber, Column: statusCol, Message: msg})
				rowValid = false
			} else {
				status = statusRaw
			}
		}
		if target != nil && rowValid {
			msg, label, code := "", firstNonEmpty(matchField.Label, matchField.Name), matchVal
			if req.UpdateRefusal == nil {
				msg = fmt.Sprintf("%s %q is taken: another record of %s has it", label, code, form.Label)
			} else if m := req.UpdateRefusal(target, status); m != "" {
				msg = fmt.Sprintf("%s %q is a record this file would update: %s", label, code, m)
			}
			if msg != "" {
				errs = append(errs, RowError{Row: row.RowNumber, Column: headerOf[matchField.Name], Message: msg})
				rowValid = false
			}
		}
		if req.RecordRefusal != nil && rowValid {
			if msg := req.RecordRefusal(data); msg != "" {
				errs = append(errs, RowError{Row: row.RowNumber, Message: msg})
				rowValid = false
			}
		}
		if matchField != nil && matchVal != "" && rowValid {
			if first, dup := matchRows[matchVal]; dup {
				errs = append(errs, RowError{Row: row.RowNumber, Column: headerOf[matchField.Name],
					Message: fmt.Sprintf("%s %q is also on row %d: a load names each record once", firstNonEmpty(matchField.Label, matchField.Name), matchVal, first)})
				rowValid = false
			} else {
				matchRows[matchVal] = row.RowNumber
			}
		}
		if replaceBy != nil && rowValid {
			if key := replaceBy.key(data); key == "" {
				empty := replaceBy.firstEmpty(data)
				errs = append(errs, RowError{Row: row.RowNumber, Column: firstNonEmpty(headerOf[empty.Name], empty.Name),
					Message: fmt.Sprintf("this load replaces the records of each %s it holds, so every row needs one", replaceBy.labels())})
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
		targetID := ""
		unchanged := false
		if target != nil {
			if status == target.Status && sameData(data, target.Data) {
				// In a load that replaces, the record may go with the period the
				// file replaces: the row is kept to add it anew then. It used to
				// be skipped here and its record deleted, so a corrected month
				// loaded again lost every row it held unchanged (2,340 of 4,335
				// ERP actuals, 2026-11-27).
				if replaceBy == nil {
					res.Unchanged++
					continue
				}
				unchanged = true
			}
			targetID = target.ID
		}
		if field, msg := checks.emptyMappedField(targetID, &RecordState{Status: status, Data: data}); field != "" {
			errs = append(errs, RowError{Row: row.RowNumber, Column: firstNonEmpty(headerOf[field], field), Message: msg})
			continue
		}
		if req.CreatorID != "" {
			code, msg, rErr := ps.postingRefusal(ctx, checks, req.CreatorID, true, targetID, &RecordState{Status: status, Data: data})
			if rErr != nil {
				return res, rErr
			}
			if code != 0 {
				errs = append(errs, RowError{Row: row.RowNumber, Message: msg})
				continue
			}
		}
		staged = append(staged, stagedRecord{data: data, status: status, target: target, unchanged: unchanged})
	}

	res.Errors = errs
	if len(errs) > 0 && !partial {
		return res, nil
	}
	keys := make([]string, 0, len(replaceKeys))
	for k := range replaceKeys {
		keys = append(keys, k)
	}
	// A row whose code a record the load replaces holds adds the record
	// anew: the old one goes with its period.
	unchangedBefore := res.Unchanged
	count := func(replaced []*crudapp.FormRecord) error {
		gone := make(map[string]bool, len(replaced))
		for _, r := range replaced {
			gone[r.ID] = true
		}
		res.Created, res.Updated, res.Unchanged = 0, 0, unchangedBefore
		for i := range staged {
			if t := staged[i].target; t != nil && gone[t.ID] {
				staged[i].target = nil
				staged[i].unchanged = false
			}
			switch {
			case staged[i].target == nil:
				res.Created++
			case staged[i].unchanged:
				res.Unchanged++
			default:
				res.Updated++
			}
		}
		if req.NewMembers != nil && res.Created > 0 {
			return req.NewMembers(res.Created)
		}
		return nil
	}
	if req.DryRun {
		var recs []*crudapp.FormRecord
		if replaceBy != nil && len(keys) > 0 {
			var rErr error
			if recs, rErr = recordsToReplace(ctx, ps.db(ctx), form, replaceBy, keys, false); rErr != nil {
				return res, rErr
			}
			if msg, rErr := ps.replaceRefusal(ctx, req, checks, replaceBy, recs); rErr != nil {
				return res, rErr
			} else if msg != "" {
				return res, &ReplaceError{Msg: msg}
			}
			res.Replaced = len(recs)
		}
		return res, count(recs)
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
	var old []*crudapp.FormRecord
	if replaceBy != nil && len(keys) > 0 {
		var rErr error
		if old, rErr = recordsToReplace(ctx, tx, form, replaceBy, keys, true); rErr != nil {
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
	if err := count(old); err != nil {
		return ImportResult{}, err
	}
	txStore := crudapp.NewStoreOn(tx)
	recs := make([]*crudapp.FormRecord, 0, len(staged))
	var moved []*crudapp.FormRecord // updated records whose status the file changed
	// The records the file created, and those it changed (with what they
	// held), for the rules they fire: recs leaves out the rows kept as they
	// were, so it does not line up with staged.
	var created []*crudapp.FormRecord
	var changed []UpdatedRecord
	for _, sr := range staged {
		if sr.target != nil && sr.unchanged {
			continue // its record stays as it is
		}
		if sr.target != nil {
			rec, uErr := ps.updateImported(ctx, tx, form, sr.target, sr.status, sr.data, req.CreatorID, &membersAdded)
			if uErr != nil {
				membersAdded = false
				return ImportResult{}, uErr
			}
			recs = append(recs, rec)
			changed = append(changed, UpdatedRecord{Old: sr.target, New: rec})
			if rec.Status != sr.target.Status {
				moved = append(moved, rec)
			}
			continue
		}
		rec, cErr := txStore.CreateRecordThen(ctx, form.ID, req.CreatorID, sr.status, sr.data, ps.AfterRecordCreated(req.CreatorID, &membersAdded))
		if cErr != nil {
			membersAdded = false
			return ImportResult{}, fmt.Errorf("create record: %w", cErr)
		}
		recs = append(recs, rec)
		created = append(created, rec)
	}
	if req.BeforeCommit != nil {
		if err := req.BeforeCommit(ctx, tx, res); err != nil {
			membersAdded = false
			return ImportResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		membersAdded = false
		return ImportResult{}, fmt.Errorf("create records: %w", err)
	}
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
	for _, rec := range created {
		ps.DispatchRecordCreated(dispatchCtx, req.AppID, req.RevisionID, form.ID, rec.ID, rec.Status, creator, true)
	}
	// A record the file moved into submitted or approved fires what a
	// record moved there by hand fires.
	for _, rec := range moved {
		ps.DispatchRecordStatus(dispatchCtx, req.AppID, req.RevisionID, form.ID, rec.ID, rec.Status, creator)
	}
	// A record the file changed fires its table's record_updated rules
	// (the ones not switched off for imports).
	for _, u := range changed {
		ps.DispatchRecordUpdated(dispatchCtx, req.AppID, req.RevisionID, form.ID, u.Old.ID, u.Old.Status, u.New.Status, u.Old.Data, u.New.Data, creator, true)
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

// sameData reports whether a and b hold the same value in every field.
func sameData(a, b map[string]any) bool {
	for k, v := range a {
		w, ok := b[k]
		if !ok || !sameValue(v, w) {
			return false
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			return false
		}
	}
	return true
}

// updateImported saves data and status on rec, the record a file's row
// updates, in the import's transaction tx — only while the record is still
// as it was read, or the whole file is refused — and keeps what a save by
// hand keeps: the member the record is (relabelled, its properties set,
// *changed when the dimension changed) and its fields' history.
func (ps *Poster) updateImported(ctx context.Context, tx pgx.Tx, form *crudapp.FormDef, rec *crudapp.FormRecord, status string, data map[string]any, userID string, changed *bool) (*crudapp.FormRecord, error) {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var updatedAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE runtime.form_record
		SET data = $2, status = NULLIF($3, '')::runtime.record_status, updated_at = now()
		WHERE id = $1::uuid AND updated_at = $4
		RETURNING updated_at`, rec.ID, dataJSON, status, rec.UpdatedAt).Scan(&updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		cf := form.CodeField()
		return nil, &ReplaceError{Msg: fmt.Sprintf("the record %v of %s changed while the file was loading; nothing was loaded: load the file again",
			rec.Data[cf.Name], form.Label)}
	}
	if err != nil {
		return nil, fmt.Errorf("update record: %w", err)
	}
	c, err := KeepMember(ctx, tx, form, data)
	if err != nil {
		return nil, err
	}
	if c && changed != nil {
		*changed = true
	}
	if err := crudapp.LogFieldChanges(ctx, tx, form, rec.ID, rec.Data, data, userID); err != nil {
		return nil, err
	}
	out := *rec
	out.Data, out.Status, out.UpdatedAt = data, status, updatedAt
	return &out, nil
}

// DispatchRecordStatus fires the rules a record moved into status by hand
// fires — form_submit into submitted, form_approval into approved — for a
// record a file's row moved there.
func (ps *Poster) DispatchRecordStatus(ctx context.Context, appID, revisionID, formID, recordID, status, userID string) {
	event := ""
	switch status {
	case crudapp.StatusSubmitted:
		event = "form_submit"
	case crudapp.StatusApproved:
		event = "form_approval"
	default:
		return
	}
	bgCtx := context.WithoutCancel(ctx)
	go func() {
		payload := map[string]string{"form_id": formID, "record_id": recordID, "status": status, "record_source": "import"}
		ps.workflowStore(ctx).DispatchEventRules(bgCtx, appID, revisionID, event, formID, userID, payload)
	}()
}
