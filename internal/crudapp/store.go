package crudapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
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
	// Code marks a text field as the record's code, typed by the person
	// (a ledger's account number, an outside system's id) where an
	// auto-code field would number the records itself: required, unique in
	// the table, and unchanged once saved. In a table keeping a dimension
	// it is the member's code (CodeField).
	Code bool `json:"code,omitempty"`
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
	// Min, Max and Whole are a number field's accepted values, as an input
	// metric's (model.metric_def.input_rules): no value below Min, none
	// above Max, whole numbers only. Every save checks them — by hand, a
	// file, a connector — naming the field: a CRM intent outside 1–5 or an
	// FTE above 1 used to come in through records unchecked, though the
	// metrics they post into refused the same value typed (2026-11-16).
	Min   *float64 `json:"min,omitempty"`
	Max   *float64 `json:"max,omitempty"`
	Whole bool     `json:"whole,omitempty"`
	// Deadline marks a date field whose date, once past, shows the record
	// as overdue — unless DoneField holds one of DoneValues (a task that is
	// completed is not overdue).
	Deadline   bool     `json:"deadline,omitempty"`
	DoneField  string   `json:"done_field,omitempty"`
	DoneValues []string `json:"done_values,omitempty"`

	// The options below are opt-in rules (2026-11-20, help desk): a field
	// without them behaves as before they existed.

	// Transitions, on a select or dimension field, are the moves its value
	// may make once a record holds one (a ticket's status New → Open →
	// Resolved → Closed): a save that changes it must make one of them. A
	// move naming Roles is made by the holders of one of those roles and by
	// the application's administrators. None: any value may follow any.
	Transitions []Transition `json:"transitions,omitempty"`
	// LocksIn, on a select or dimension field, are its values in which the
	// record is read-only: a save may change this field alone (by its
	// transitions), as a closed ticket is reopened rather than edited.
	LocksIn []string `json:"locks_in,omitempty"`
	// RequiredWhen makes the field required while the record meets the
	// condition (a resolution summary once the status is Resolved).
	RequiredWhen *RowCondition `json:"required_when,omitempty"`
	// StampWhen, on a date field, sets it to the day of the save that
	// brings the record to meet the condition (the day a ticket is
	// resolved). The server keeps it: a save does not change it, except a
	// new record arriving with one (records loaded from elsewhere keep
	// their dates).
	StampWhen *RowCondition `json:"stamp_when,omitempty"`
	// ActiveFlag marks a boolean field of a table keeping a dimension as
	// whether its record's member is active: a member whose record has it
	// unticked stays on the records naming it, and is offered for no new
	// choice.
	ActiveFlag bool `json:"active_flag,omitempty"`
	// Roles, on a person field, are the roles whose holders may be chosen
	// (none: anyone who reaches the application). Notify tells the person a
	// save names in it, in the console's notifications — on a dimension
	// field naming a member a table keeps, the person that member's record
	// names in its person field (a ticket's assignee is an agent record).
	Roles  []string `json:"roles,omitempty"`
	Notify bool     `json:"notify,omitempty"`
}

// Transition is a move a field's value may make: from any value of From
// to any value of To, by the holders of Roles (none: by anyone who may
// change the record).
type Transition struct {
	From  []string `json:"from"`
	To    []string `json:"to"`
	Roles []string `json:"roles,omitempty"`
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

// Who may delete a table's records (model.form_def.delete_access).
const (
	DeleteEditors = ""
	DeleteAdmins  = "admins"
)

// Who sees a table's records (model.form_def.record_scope).
const (
	ScopeByAccess = ""
	ScopeOpen     = "open"
)

// What a file row naming an auto code does (model.form_def.existing_codes).
const (
	CodesIgnored = ""
	CodesRefuse  = "refuse"
	CodesUpdate  = "update"
)

// IsOpen reports whether everyone who opens the table sees every record,
// and saves post within the access of the person saving.
func (f *FormDef) IsOpen() bool { return f != nil && f.RecordScope == ScopeOpen }

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
	// MemberLabelWithCode labels each member with its record's code before
	// its label field ("IC03 · Intragroup loan principal"), so records with
	// the same text tell apart in grids (migration 183).
	MemberLabelWithCode bool `json:"member_label_with_code,omitempty"`
	// EditAccess is who may change and delete the table's records:
	// EditCreator (their creator and the application's administrators) or
	// EditEveryone (everyone who sees them).
	EditAccess string `json:"edit_access"`
	// CreateRoles names who may add records, as a workflow's start roles
	// do; none: anyone who reaches the application. Its administrators
	// always may (migration 157).
	CreateRoles []string `json:"create_roles"`
	// DeleteAccess is who may delete the table's records: DeleteEditors
	// (whoever may change a record, as before migration 192) or
	// DeleteAdmins (the application's administrators, and those who may
	// change a record and hold one of DeleteRoles).
	DeleteAccess string   `json:"delete_access,omitempty"`
	DeleteRoles  []string `json:"delete_roles,omitempty"`
	// RecordScope is who sees the table's records (migration 193):
	// ScopeByAccess (members' access rules narrow them, and a record posts
	// within its creator's access) or ScopeOpen (everyone who opens the
	// table sees every record, and a save posts within the access of the
	// person saving it).
	RecordScope string `json:"record_scope,omitempty"`
	// ExistingCodes is what a file row giving the table's auto code a value
	// does: CodesRefuse (refused) or CodesUpdate (updates the record holding
	// it); "" — a table made before migration 193 — ignores the value and
	// adds a record, as such tables always did.
	ExistingCodes string    `json:"existing_codes,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	// ModelKeeps: some table of the table's model keeps a dimension, so its
	// dimension fields may link to records (read with the table, so a model
	// without any pays nothing to find that out).
	ModelKeeps bool `json:"-"`
}

// CodeField is the table's code field, nil when it has none: its first
// auto-code field, else the text field marked as the code (FormField.Code)
// — the code of the member a record is, in a table that keeps a dimension.
func (f *FormDef) CodeField() *FormField {
	for i := range f.Fields {
		if f.Fields[i].Type == FieldAutoCode {
			return &f.Fields[i]
		}
	}
	for i := range f.Fields {
		if f.Fields[i].Code {
			return &f.Fields[i]
		}
	}
	return nil
}

// TypedCodeField is the text field marked as the record's code, nil when
// the table has none (or numbers its records with an auto-code field).
func (f *FormDef) TypedCodeField() *FormField {
	if c := f.CodeField(); c != nil && c.Type != FieldAutoCode {
		return c
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
// label field, else its code — the code first when the table says so
// (MemberLabelWithCode).
func (f *FormDef) MemberLabel(data map[string]any) string {
	code := f.MemberCode(data)
	if f.MemberLabelField != "" {
		if v, ok := data[f.MemberLabelField]; ok && v != nil {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				if f.MemberLabelWithCode && code != "" && s != code {
					return code + " · " + s
				}
				return s
			}
		}
	}
	return code
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

// ErrCodeTaken refuses a record whose typed code (FormField.Code) another
// record of its table has.
var ErrCodeTaken = errors.New("code taken")

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
	var existingCodes string
	err = s.db.QueryRow(ctx, `
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields, existing_codes)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5, 'refuse')
		ON CONFLICT (model_id, revision_id, name) DO UPDATE SET label = EXCLUDED.label, fields = EXCLUDED.fields
		RETURNING id::text, created_at, existing_codes
	`, modelID, revisionID, name, label, fieldsJSON).Scan(&id, &createdAt, &existingCodes)
	if err != nil {
		return nil, fmt.Errorf("create form: %w", err)
	}

	return &FormDef{
		ID:            id,
		ModelID:       modelID,
		Name:          name,
		Label:         label,
		Fields:        fields,
		EditAccess:    EditCreator,
		ExistingCodes: existingCodes,
		CreatedAt:     createdAt,
	}, nil
}

// formDefColumns are what GetForm and ListForms read of a model.form_def row.
const formDefColumns = `id::text, model_id::text, name, label, fields, own_records_only,
	COALESCE(keeps_dimension_id::text, ''), COALESCE(member_label_field, ''), COALESCE(member_root_label, ''), edit_access, create_roles, created_at,
	EXISTS (SELECT 1 FROM model.form_def k WHERE k.model_id = form_def.model_id AND k.keeps_dimension_id IS NOT NULL),
	member_label_with_code, delete_access, delete_roles, record_scope, existing_codes`

func (s *Store) GetForm(ctx context.Context, formID string) (*FormDef, error) {
	var f FormDef
	var fieldsJSON []byte

	err := s.db.QueryRow(ctx, `
		SELECT `+formDefColumns+`
		FROM model.form_def WHERE id = $1::uuid
	`, formID).Scan(&f.ID, &f.ModelID, &f.Name, &f.Label, &fieldsJSON, &f.OwnRecordsOnly, &f.KeepsDimensionID, &f.MemberLabelField, &f.MemberRootLabel, &f.EditAccess, &f.CreateRoles, &f.CreatedAt, &f.ModelKeeps, &f.MemberLabelWithCode, &f.DeleteAccess, &f.DeleteRoles, &f.RecordScope, &f.ExistingCodes)
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
		if err := rows.Scan(&f.ID, &f.ModelID, &f.Name, &f.Label, &fieldsJSON, &f.OwnRecordsOnly, &f.KeepsDimensionID, &f.MemberLabelField, &f.MemberRootLabel, &f.EditAccess, &f.CreateRoles, &f.CreatedAt, &f.ModelKeeps, &f.MemberLabelWithCode, &f.DeleteAccess, &f.DeleteRoles, &f.RecordScope, &f.ExistingCodes); err != nil {
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
	if cf := form.TypedCodeField(); cf != nil {
		// Two saves of one code at once both passed the check before the
		// save: the second, waiting for the first here, finds its record.
		code, _ := data[cf.Name].(string)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, formID, code); err != nil {
			return nil, err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id = $1::uuid AND data->>$2 = $3`,
			formID, cf.Name, code).Scan(&n); err != nil {
			return nil, err
		}
		if n > 1 {
			return nil, fmt.Errorf("%w: %s %q is taken by another record of %s", ErrCodeTaken, firstNonEmpty(cf.Label, cf.Name), code, form.Label)
		}
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
	_, err := s.UpdateRecordSince(ctx, recordID, fromStatus, nil, status, data, then)
	return err
}

// UpdateRecordSince is UpdateRecordThen that, given readAt, also writes
// only while the record is unchanged since it was read: its updated_at is
// still readAt (ErrRecordChanged otherwise). A save that sends the time it
// read the record at is not written over someone else's change in between.
// It returns the record's new updated_at, what the next such save sends.
func (s *Store) UpdateRecordSince(ctx context.Context, recordID, fromStatus string, readAt *time.Time, status string, data map[string]any, then func(ctx context.Context, tx dbx.DB) error) (time.Time, error) {
	var updatedAt time.Time
	if data == nil {
		data = map[string]any{}
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return updatedAt, err
	}
	db := s.db
	var tx pgx.Tx
	if then != nil {
		if tx, err = s.db.Begin(ctx); err != nil {
			return updatedAt, err
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
		db = tx
	}
	err = db.QueryRow(ctx, `
		UPDATE runtime.form_record
		SET data = $2, status = NULLIF($3,'')::runtime.record_status, updated_at = now()
		WHERE id = $1::uuid AND status IS NOT DISTINCT FROM NULLIF($4,'')::runtime.record_status
		  AND ($5::timestamptz IS NULL OR updated_at = $5::timestamptz)
		RETURNING updated_at
	`, recordID, dataJSON, status, fromStatus, readAt).Scan(&updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return updatedAt, ErrRecordChanged
	}
	if err != nil {
		return updatedAt, fmt.Errorf("update record: %w", err)
	}
	if then == nil {
		return updatedAt, nil
	}
	if err := then(ctx, tx); err != nil {
		return updatedAt, err
	}
	return updatedAt, tx.Commit(ctx)
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

// fieldOptionKeys are the keys a field definition may carry: FormField's.
var fieldOptionKeys = func() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(FormField{})
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}()

// UnknownFieldOptions says which key of a table's field definitions is no
// option of a field (nil when none is): {"prefix": "IC"} on an auto-code
// field used to be dropped in silence, and the codes came out 0001 instead
// of IC01 (2026-11-13). fields is the request's "fields" array as sent.
func UnknownFieldOptions(fields json.RawMessage) error {
	if len(fields) == 0 || string(fields) == "null" {
		return nil
	}
	var defs []map[string]json.RawMessage
	if err := json.Unmarshal(fields, &defs); err != nil {
		return nil // the typed decode says what is wrong with it
	}
	for i, d := range defs {
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var bad []string
		for _, k := range keys {
			if fieldOptionKeys[k] {
				continue
			}
			item := fmt.Sprintf("%q", k)
			for _, known := range sortedFieldOptionKeys() {
				if strings.Contains(known, strings.ToLower(k)) {
					item += fmt.Sprintf(" (did you mean %q?)", known)
					break
				}
			}
			bad = append(bad, item)
		}
		if len(bad) > 0 {
			name := fmt.Sprintf("field %d", i+1)
			var n string
			if json.Unmarshal(d["name"], &n) == nil && n != "" {
				name = fmt.Sprintf("field %q", n)
			}
			return fmt.Errorf("%s: no option of a field: %s", name, strings.Join(bad, ", "))
		}
	}
	return nil
}

func sortedFieldOptionKeys() []string {
	out := make([]string, 0, len(fieldOptionKeys))
	for k := range fieldOptionKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
