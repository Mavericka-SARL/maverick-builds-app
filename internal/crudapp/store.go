package crudapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// FormField is one field of a table of records (model.form_def.fields).
type FormField struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // text, number, select, date, boolean, dimension, metric, autocode, status
	Required bool     `json:"required"`
	Options  []string `json:"options,omitempty"`
	// dimension type
	DimensionID    string   `json:"dimension_id,omitempty"`
	AllowedMembers []string `json:"allowed_members,omitempty"`
	// metric type
	MetricID   string `json:"metric_id,omitempty"`
	ValueField string `json:"value_field,omitempty"` // name of sibling field that holds the numeric amount
	// autocode type: the code a new record is given is CodePrefix followed
	// by the next number, padded with zeros to CodeDigits (EXP-0042).
	CodePrefix string `json:"code_prefix,omitempty"`
	CodeDigits int    `json:"code_digits,omitempty"`

	// Default is the value a new record starts with when it is given none:
	// the form's starting value, and what the server stores for a record
	// created without the field.
	Default any `json:"default,omitempty"`
	// RequiredWith names another field this one is filled in together with:
	// when either holds a value, both must (a next action's description and
	// its date).
	RequiredWith string `json:"required_with,omitempty"`
	// Hidden fields are kept and saved but not shown in the table's forms,
	// lists and boards.
	Hidden bool `json:"hidden,omitempty"`
	// Core fields cannot be removed from the table while they are marked
	// core.
	Core bool `json:"core,omitempty"`
	// History keeps each value the field takes, who gave it and when
	// (runtime.record_field_history), shown with the record.
	History bool `json:"history,omitempty"`
	// OnDelete is what happens to this table's records when the record
	// they name through a dimension field is deleted, where the dimension
	// is kept by a table: "block" (the default: that record cannot be
	// deleted while one names it), "cascade" (they are deleted with it) or
	// "clear" (the field is emptied).
	OnDelete string `json:"on_delete,omitempty"`
	// Format shows a number field's values: "money" (two decimals, grouped
	// thousands); "" plain.
	Format string `json:"format,omitempty"`
	// Deadline marks a date field whose date, once past, shows the record
	// as overdue — unless DoneField holds one of DoneValues (a task that is
	// completed is not overdue).
	Deadline   bool     `json:"deadline,omitempty"`
	DoneField  string   `json:"done_field,omitempty"`
	DoneValues []string `json:"done_values,omitempty"`
}

// Delete rules of a dimension field naming a kept dimension's member.
const (
	OnDeleteBlock   = "block"
	OnDeleteCascade = "cascade"
	OnDeleteClear   = "clear"
)

// DeleteRule is f's rule, the default when it names none.
func (f FormField) DeleteRule() string {
	if f.OnDelete == "" {
		return OnDeleteBlock
	}
	return f.OnDelete
}

// Who may change a table's records (model.form_def.edit_access).
const (
	EditCreator  = "creator"
	EditEveryone = "everyone"
)

// Field types the server gives values to rather than the person saving a
// record: an autocode field's code, and a status field's status (which
// lives in the record's status, not its data).
const (
	FieldAutoCode = "autocode"
	FieldStatus   = "status"
)

// FormDef is a table of records: its fields, and whether each person sees
// only the records they created.
type FormDef struct {
	ID      string      `json:"id"`
	ModelID string      `json:"model_id"`
	Name    string      `json:"name"`
	Label   string      `json:"label"`
	Fields  []FormField `json:"fields"`
	// OwnRecordsOnly: a person who does not administer the table's
	// application sees only the records they created.
	OwnRecordsOnly bool `json:"own_records_only"`
	// KeepsDimensionID is the dimension whose members are this table's
	// records ("" when it keeps none): each record's auto-code is a member's
	// code and its MemberLabelField the member's label.
	KeepsDimensionID string `json:"keeps_dimension_id,omitempty"`
	MemberLabelField string `json:"member_label_field,omitempty"`
	// MemberRootLabel, when set, labels the total member (code RootMemberCode)
	// every record's member sits under.
	MemberRootLabel string `json:"member_root_label,omitempty"`
	// EditAccess is who may change and delete the table's records:
	// EditCreator (their creator and the application's administrators) or
	// EditEveryone (everyone who sees them).
	EditAccess string    `json:"edit_access"`
	CreatedAt  time.Time `json:"created_at"`
	// ModelKeeps: some table of the table's model keeps a dimension, so its
	// dimension fields may link to records (read with the table, so a model
	// without any pays nothing to find that out).
	ModelKeeps bool `json:"-"`
}

// CodeField is the table's first auto-code field, nil when it has none:
// the code of the member a record is, in a table that keeps a dimension.
func (f *FormDef) CodeField() *FormField {
	for i := range f.Fields {
		if f.Fields[i].Type == FieldAutoCode {
			return &f.Fields[i]
		}
	}
	return nil
}

// Field is the table's field named name, nil when it has none.
func (f *FormDef) Field(name string) *FormField {
	for i := range f.Fields {
		if f.Fields[i].Name == name {
			return &f.Fields[i]
		}
	}
	return nil
}

// RootMemberCode is the code of the total member a kept dimension's
// members sit under, when its table names one (MemberRootLabel).
const RootMemberCode = "ALL"

// KeepsDimension reports whether the table's records are a dimension's
// members.
func (f *FormDef) KeepsDimension() bool { return f.KeepsDimensionID != "" && f.CodeField() != nil }

// MemberCode is the code of the member rec is, "" when the table keeps no
// dimension or rec has no code.
func (f *FormDef) MemberCode(data map[string]any) string {
	if !f.KeepsDimension() {
		return ""
	}
	s, _ := data[f.CodeField().Name].(string)
	return s
}

// MemberLabel is the label of the member a record holding data is: its
// label field, else its code.
func (f *FormDef) MemberLabel(data map[string]any) string {
	if f.MemberLabelField != "" {
		if v, ok := data[f.MemberLabelField]; ok && v != nil {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}
	return f.MemberCode(data)
}

// EveryoneEdits reports whether everyone who sees the table's records may
// change them.
func (f *FormDef) EveryoneEdits() bool { return f.EditAccess == EditEveryone }

// StatusField is the table's status field, nil when it has none: a table
// without one gives its records no status.
func (f *FormDef) StatusField() *FormField {
	for i := range f.Fields {
		if f.Fields[i].Type == FieldStatus {
			return &f.Fields[i]
		}
	}
	return nil
}

// HasStatus reports whether the table's records have a status.
func (f *FormDef) HasStatus() bool { return f.StatusField() != nil }

type FormRecord struct {
	ID     string         `json:"id"`
	FormID string         `json:"form_id"`
	Data   map[string]any `json:"data"`
	// Status is "" for a record without one: every record of a table
	// without a status field, and one saved before its table got one.
	Status string `json:"status"`
	// CreatedBy is the user who created the record; nil for a legacy row
	// whose creator was never recorded or has been deleted.
	CreatedBy *string   `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Permissions is what the caller the record is served to may do to it
	// (RecordAccess.Permissions). The gateway sets it on every record it
	// returns; the store never does.
	Permissions *RecordPermissions `json:"permissions,omitempty"`
}

// ErrRecordNotFound is returned when a record does not exist.
var ErrRecordNotFound = errors.New("record not found")

// ErrRecordChanged is returned by the conditional writes (UpdateRecordFrom,
// DeleteRecordFrom) when the record is gone or no longer in the status the
// caller decided on.
var ErrRecordChanged = errors.New("record changed since it was read")

type Store struct {
	pool *pgxpool.Pool
	db   dbx.DB // what the store\'s statements run on: the pool, or a transaction
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, db: pool} }

// NewStoreOn is NewStore on a pool or a transaction (dbx.DB): built on a
// transaction it writes nothing the transaction does not keep. Pool() is
// nil unless db is a pool.
func NewStoreOn(db dbx.DB) *Store {
	s := &Store{db: db}
	if p, ok := db.(*pgxpool.Pool); ok {
		s.pool = p
	}
	return s
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// CreateForm creates (or upserts by name) a form scoped to revisionID.
// An empty revisionID leaves the form revision-global (visible in every
// revision) for callers that predate revision isolation.
func (s *Store) CreateForm(ctx context.Context, modelID, revisionID, name, label string, fields []FormField) (*FormDef, error) {
	// A nil slice marshals to the JSON scalar `null`, not `[]` — stored as a
	// valid (non-SQL-NULL) jsonb value that later breaks any
	// jsonb_array_elements(fields) reader, e.g. the revision-duplication
	// copy steps (both the developer-console handler's and the AI
	// assistant's). Normalize so model.form_def.fields is always a real
	// JSON array.
	if fields == nil {
		fields = []FormField{}
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}

	var id string
	var createdAt time.Time
	err = s.db.QueryRow(ctx, `
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5)
		ON CONFLICT (model_id, revision_id, name) DO UPDATE SET label = EXCLUDED.label, fields = EXCLUDED.fields
		RETURNING id::text, created_at
	`, modelID, revisionID, name, label, fieldsJSON).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("create form: %w", err)
	}

	return &FormDef{
		ID:         id,
		ModelID:    modelID,
		Name:       name,
		Label:      label,
		Fields:     fields,
		EditAccess: EditCreator,
		CreatedAt:  createdAt,
	}, nil
}

// formDefColumns are what GetForm and ListForms read of a model.form_def row.
const formDefColumns = `id::text, model_id::text, name, label, fields, own_records_only,
	COALESCE(keeps_dimension_id::text, ''), COALESCE(member_label_field, ''), COALESCE(member_root_label, ''), edit_access, created_at,
	EXISTS (SELECT 1 FROM model.form_def k WHERE k.model_id = form_def.model_id AND k.keeps_dimension_id IS NOT NULL)`

func (s *Store) GetForm(ctx context.Context, formID string) (*FormDef, error) {
	var f FormDef
	var fieldsJSON []byte

	err := s.db.QueryRow(ctx, `
		SELECT `+formDefColumns+`
		FROM model.form_def WHERE id = $1::uuid
	`, formID).Scan(&f.ID, &f.ModelID, &f.Name, &f.Label, &fieldsJSON, &f.OwnRecordsOnly, &f.KeepsDimensionID, &f.MemberLabelField, &f.MemberRootLabel, &f.EditAccess, &f.CreatedAt, &f.ModelKeeps)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("form %s not found", formID)
	}
	if err != nil {
		return nil, err
	}

	_ = json.Unmarshal(fieldsJSON, &f.Fields)
	return &f, nil
}

// ListForms lists a revision's forms plus revision-global (NULL revision)
// ones. An empty revisionID lists every form for the model.
func (s *Store) ListForms(ctx context.Context, modelID, revisionID string) ([]*FormDef, error) {
	rows, err := s.db.Query(ctx, `
		SELECT `+formDefColumns+`
		FROM model.form_def
		WHERE model_id = $1::uuid
		  AND ($2 = '' OR revision_id IS NULL OR revision_id::text = $2)
		ORDER BY created_at
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var forms []*FormDef
	for rows.Next() {
		var f FormDef
		var fieldsJSON []byte
		if err := rows.Scan(&f.ID, &f.ModelID, &f.Name, &f.Label, &fieldsJSON, &f.OwnRecordsOnly, &f.KeepsDimensionID, &f.MemberLabelField, &f.MemberRootLabel, &f.EditAccess, &f.CreatedAt, &f.ModelKeeps); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(fieldsJSON, &f.Fields)
		forms = append(forms, &f)
	}
	return forms, rows.Err()
}

// FormChange is what an UpdateForm changed that its caller acts on.
type FormChange struct {
	// StatusChanged: the table gained or lost its status field, so which of
	// its records count in their metrics changed — the caller re-posts them.
	StatusChanged bool
	// CodesAssigned: records given a code by an auto-code field the update
	// added.
	CodesAssigned int
}

// UpdateForm saves a table's name, label and fields. Records without a
// value in an auto-code field — every record, when the field is new — are
// given codes, oldest first, so every record of the table is identifiable.
func (s *Store) UpdateForm(ctx context.Context, formID, name, label string, fields []FormField) (FormChange, error) {
	var change FormChange
	if fields == nil {
		fields = []FormField{}
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return change, err
	}
	before, err := s.GetForm(ctx, formID)
	if err != nil {
		return change, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return change, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx,
		`UPDATE model.form_def SET name=$2, label=$3, fields=$4 WHERE id=$1::uuid`,
		formID, name, label, fieldsJSON); err != nil {
		return change, err
	}
	after := *before
	after.Fields = fields
	change.StatusChanged = before.HasStatus() != after.HasStatus()
	for _, f := range fields {
		if f.Type != FieldAutoCode {
			continue
		}
		n, err := assignMissingCodes(ctx, tx, formID, f)
		if err != nil {
			return change, err
		}
		change.CodesAssigned += n
	}
	return change, tx.Commit(ctx)
}

// SetOwnRecordsOnly sets whether the table shows each person only the
// records they created.
func (s *Store) SetOwnRecordsOnly(ctx context.Context, formID string, on bool) error {
	_, err := s.db.Exec(ctx, `UPDATE model.form_def SET own_records_only=$2 WHERE id=$1::uuid`, formID, on)
	return err
}

// SetEditAccess sets who may change and delete the table's records
// (EditCreator or EditEveryone).
func (s *Store) SetEditAccess(ctx context.Context, formID, access string) error {
	if access != EditCreator && access != EditEveryone {
		return fmt.Errorf("edit_access is %q or %q, not %q", EditCreator, EditEveryone, access)
	}
	_, err := s.db.Exec(ctx, `UPDATE model.form_def SET edit_access=$2 WHERE id=$1::uuid`, formID, access)
	return err
}

// DeleteForm deletes a form, its records and its mappings, and the totals
// its mappings posted into their metrics, returning the metrics whose cells
// changed for the caller to recalculate.
func (s *Store) DeleteForm(ctx context.Context, formID string) ([]PostedMetric, error) {
	var mappingIDs []string
	rows, err := s.db.Query(ctx, `SELECT id::text FROM model.form_metric_mapping WHERE form_id=$1::uuid`, formID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		mappingIDs = append(mappingIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	posted, err := s.DeleteMappings(ctx, mappingIDs)
	if err != nil {
		return nil, err
	}
	// SYNC-01 cascade: drop form widgets referencing this form (ref_id is
	// bare TEXT, no FK — a dangling form widget rendered an empty error box).
	_, _ = s.db.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE ref_id = $1`, formID)
	_, err = s.db.Exec(ctx, `DELETE FROM model.form_def WHERE id=$1::uuid`, formID)
	return posted, err
}

// PostedMetric is a metric of a revision a form mapping posted into.
type PostedMetric struct{ ModelID, RevisionID, MetricID string }

// DeleteMappings deletes the mappings and withdraws the totals they posted,
// in one transaction. Deleting the rows first takes their row locks: a
// recompute still running for one of them holds the row FOR SHARE, so it
// finishes before the totals are withdrawn, and one that starts later finds
// the mapping gone and writes nothing. Withdrawing in one statement and
// deleting in another left a window in which a re-apply still in flight
// posted the total back under the id of a mapping that no longer existed,
// where nothing could take it out (public CI, 2026-10-06).
func (s *Store) DeleteMappings(ctx context.Context, mappingIDs []string) ([]PostedMetric, error) {
	if len(mappingIDs) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM model.form_metric_mapping WHERE id = ANY($1::uuid[])`, mappingIDs); err != nil {
		return nil, fmt.Errorf("delete mappings: %w", err)
	}
	posted, err := NewStoreOn(tx).DeleteMappingPostings(ctx, mappingIDs, "")
	if err != nil {
		return nil, err
	}
	return posted, tx.Commit(ctx)
}

// DeleteMappingPostings removes the totals the mappings posted: the
// runtime.fact_input rows tagged with a mapping's id (source_ref, which has
// no foreign key). Deleting a mapping or its form used to leave them, so a
// metric went on showing money from a mapping that no longer existed, and
// nothing in the console could take it out. exceptMetricID keeps that
// metric's rows: a mapping retargeted to it posts there again. The
// mappings' own posting rows go with the mapping (ON DELETE CASCADE).
func (s *Store) DeleteMappingPostings(ctx context.Context, mappingIDs []string, exceptMetricID string) ([]PostedMetric, error) {
	if len(mappingIDs) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// The archive trigger on runtime.fact_input records the reason.
	if _, err := tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'form_mapping_removed'`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		WITH gone AS (
			DELETE FROM runtime.fact_input
			WHERE source_ref = ANY($1::uuid[]) AND ($2 = '' OR metric_id <> NULLIF($2,'')::uuid)
			RETURNING model_id, revision_id, metric_id)
		SELECT DISTINCT model_id::text, revision_id::text, metric_id::text FROM gone`,
		mappingIDs, exceptMetricID)
	if err != nil {
		return nil, fmt.Errorf("delete mapping postings: %w", err)
	}
	var posted []PostedMetric
	for rows.Next() {
		var p PostedMetric
		if err := rows.Scan(&p.ModelID, &p.RevisionID, &p.MetricID); err != nil {
			rows.Close()
			return nil, err
		}
		posted = append(posted, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// On a store built on a transaction Begin is a savepoint, and SET LOCAL
	// would outlive it: later deletes in that transaction would be archived
	// under this reason.
	if _, err := tx.Exec(ctx, `SELECT set_config('mvx.delete_reason', '', true)`); err != nil {
		return nil, err
	}
	return posted, tx.Commit(ctx)
}

// CreateRecord creates a record in the status a new record of its table
// starts in: a draft when the table has a status field, no status otherwise.
func (s *Store) CreateRecord(ctx context.Context, formID, userID string, data map[string]any) (*FormRecord, error) {
	form, err := s.GetForm(ctx, formID)
	if err != nil {
		return nil, err
	}
	status := ""
	if form.HasStatus() {
		status = StatusDraft
	}
	return s.CreateRecordWithStatus(ctx, formID, userID, status, data)
}

// CreateRecordWithStatus creates a record in status ("" for none). Who may
// create a record in which status is RecordAccess.CanCreate's, decided by
// the caller. Each auto-code field of the table gives the record the next
// code, whatever data holds for it.
func (s *Store) CreateRecordWithStatus(ctx context.Context, formID, userID, status string, data map[string]any) (*FormRecord, error) {
	return s.CreateRecordThen(ctx, formID, userID, status, data, nil)
}

// AfterCreate runs inside a record's creating transaction, once the record
// row exists: what fails in it undoes the record (the member a record of a
// table keeping a dimension is, its first history).
type AfterCreate func(ctx context.Context, tx dbx.DB, form *FormDef, rec *FormRecord) error

// CreateRecordThen is CreateRecordWithStatus running then, when not nil, in
// the same transaction.
func (s *Store) CreateRecordThen(ctx context.Context, formID, userID, status string, data map[string]any, then AfterCreate) (*FormRecord, error) {
	form, err := s.GetForm(ctx, formID)
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = map[string]any{}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, f := range form.Fields {
		switch f.Type {
		case FieldAutoCode:
			code, err := nextCode(ctx, tx, formID, f)
			if err != nil {
				return nil, err
			}
			data[f.Name] = code
		case FieldStatus:
			delete(data, f.Name) // the status is the record's, not a value in its data
		}
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	var id string
	var createdAt, updatedAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO runtime.form_record (form_id, data, status, created_by)
		VALUES ($1::uuid, $2, NULLIF($3,'')::runtime.record_status, NULLIF($4,'')::uuid)
		RETURNING id::text, created_at, updated_at
	`, formID, dataJSON, status, userID).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("create record: %w", err)
	}
	var createdBy *string
	if userID != "" {
		createdBy = &userID
	}
	rec := &FormRecord{
		ID:        id,
		FormID:    formID,
		Data:      data,
		Status:    status,
		CreatedBy: createdBy,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
	if then != nil {
		if err := then(ctx, tx, form, rec); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Store) ListRecords(ctx context.Context, formID string, limit int) ([]*FormRecord, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id::text, form_id::text, data, COALESCE(status::text,''), created_by::text, created_at, updated_at
		FROM runtime.form_record WHERE form_id = $1::uuid
		ORDER BY created_at DESC LIMIT $2
	`, formID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []*FormRecord
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// RecordPosition is a place in a form's record order (newest first, id the
// tie-breaker): ListRecordsPage continues after it.
type RecordPosition struct {
	CreatedAt time.Time
	ID        string
}

// ListRecordsPage lists up to limit of a form's records, newest first,
// after `after` (from the start when nil). The order is total — created_at
// alone is not, and two records of one import share it — so paging through
// it reaches every record exactly once.
func (s *Store) ListRecordsPage(ctx context.Context, formID string, after *RecordPosition, limit int) ([]*FormRecord, error) {
	var afterAt *time.Time
	var afterID *string
	if after != nil {
		afterAt, afterID = &after.CreatedAt, &after.ID
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, form_id::text, data, COALESCE(status::text,''), created_by::text, created_at, updated_at
		FROM runtime.form_record
		WHERE form_id = $1::uuid
		  AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
		ORDER BY created_at DESC, id DESC LIMIT $4
	`, formID, afterAt, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []*FormRecord
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *Store) GetRecord(ctx context.Context, recordID string) (*FormRecord, error) {
	var r FormRecord
	var dataJSON []byte
	err := s.db.QueryRow(ctx, `
		SELECT id::text, form_id::text, data, COALESCE(status::text,''), created_by::text, created_at, updated_at
		FROM runtime.form_record WHERE id = $1::uuid
	`, recordID).Scan(&r.ID, &r.FormID, &dataJSON, &r.Status, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("record %s: %w", recordID, ErrRecordNotFound)
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(dataJSON, &r.Data)
	return &r, nil
}

func (s *Store) UpdateRecord(ctx context.Context, recordID, status string, data map[string]any) error {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return err
	}

	tag, err := s.db.Exec(ctx, `
		UPDATE runtime.form_record
		SET data = $2, status = NULLIF($3,'')::runtime.record_status, updated_at = now()
		WHERE id = $1::uuid
	`, recordID, dataJSON, status)
	if err != nil {
		return fmt.Errorf("update record: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record %s not found", recordID)
	}
	return nil
}

// UpdateRecordFrom saves a record's data and status only while it is still
// in fromStatus, the status its caller's permission check was made against:
// a record decided in the meantime (an administrator approved it while its
// creator was saving) is not overwritten. ErrRecordChanged otherwise. A nil
// data saves an empty object.
func (s *Store) UpdateRecordFrom(ctx context.Context, recordID, fromStatus, status string, data map[string]any) error {
	return s.UpdateRecordThen(ctx, recordID, fromStatus, status, data, nil)
}

// UpdateRecordThen is UpdateRecordFrom running then, when not nil, in the
// same transaction once the record is saved: what fails in it undoes the
// save (the member a record of a table keeping a dimension is, its history).
func (s *Store) UpdateRecordThen(ctx context.Context, recordID, fromStatus, status string, data map[string]any, then func(ctx context.Context, tx dbx.DB) error) error {
	if data == nil {
		data = map[string]any{}
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return err
	}
	db := s.db
	var tx pgx.Tx
	if then != nil {
		if tx, err = s.db.Begin(ctx); err != nil {
			return err
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
		db = tx
	}
	tag, err := db.Exec(ctx, `
		UPDATE runtime.form_record
		SET data = $2, status = NULLIF($3,'')::runtime.record_status, updated_at = now()
		WHERE id = $1::uuid AND status IS NOT DISTINCT FROM NULLIF($4,'')::runtime.record_status
	`, recordID, dataJSON, status, fromStatus)
	if err != nil {
		return fmt.Errorf("update record: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRecordChanged
	}
	if then == nil {
		return nil
	}
	if err := then(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteRecordFrom deletes a record only while it is still in fromStatus
// (UpdateRecordFrom's guard). ErrRecordChanged otherwise.
func (s *Store) DeleteRecordFrom(ctx context.Context, recordID, fromStatus string) error {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM runtime.form_record WHERE id = $1::uuid AND status IS NOT DISTINCT FROM NULLIF($2,'')::runtime.record_status`,
		recordID, fromStatus)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRecordChanged
	}
	return nil
}

func (s *Store) DeleteRecord(ctx context.Context, recordID string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM runtime.form_record WHERE id = $1::uuid`, recordID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record %s not found", recordID)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row rowScanner) (*FormRecord, error) {
	var r FormRecord
	var dataJSON []byte
	if err := row.Scan(&r.ID, &r.FormID, &dataJSON, &r.Status, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(dataJSON, &r.Data)
	return &r, nil
}
