package crudapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type FormField struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // text, number, select, date, boolean, dimension, metric
	Required bool     `json:"required"`
	Options  []string `json:"options,omitempty"`
	// dimension type
	DimensionID    string   `json:"dimension_id,omitempty"`
	AllowedMembers []string `json:"allowed_members,omitempty"`
	// metric type
	MetricID   string `json:"metric_id,omitempty"`
	ValueField string `json:"value_field,omitempty"` // name of sibling field that holds the numeric amount
}

type FormDef struct {
	ID        string      `json:"id"`
	ModelID   string      `json:"model_id"`
	Name      string      `json:"name"`
	Label     string      `json:"label"`
	Fields    []FormField `json:"fields"`
	CreatedAt time.Time   `json:"created_at"`
}

type FormRecord struct {
	ID        string         `json:"id"`
	FormID    string         `json:"form_id"`
	Data      map[string]any `json:"data"`
	Status    string         `json:"status"`
	CreatedBy string         `json:"created_by"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

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
	err = s.pool.QueryRow(ctx, `
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5)
		ON CONFLICT (model_id, revision_id, name) DO UPDATE SET label = EXCLUDED.label, fields = EXCLUDED.fields
		RETURNING id::text, created_at
	`, modelID, revisionID, name, label, fieldsJSON).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("create form: %w", err)
	}

	return &FormDef{
		ID:        id,
		ModelID:   modelID,
		Name:      name,
		Label:     label,
		Fields:    fields,
		CreatedAt: createdAt,
	}, nil
}

func (s *Store) GetForm(ctx context.Context, formID string) (*FormDef, error) {
	var f FormDef
	var fieldsJSON []byte

	err := s.pool.QueryRow(ctx, `
		SELECT id::text, model_id::text, name, label, fields, created_at
		FROM model.form_def WHERE id = $1::uuid
	`, formID).Scan(&f.ID, &f.ModelID, &f.Name, &f.Label, &fieldsJSON, &f.CreatedAt)
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
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, model_id::text, name, label, fields, created_at
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
		if err := rows.Scan(&f.ID, &f.ModelID, &f.Name, &f.Label, &fieldsJSON, &f.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(fieldsJSON, &f.Fields)
		forms = append(forms, &f)
	}
	return forms, rows.Err()
}

func (s *Store) UpdateForm(ctx context.Context, formID, name, label string, fields []FormField) error {
	if fields == nil {
		fields = []FormField{}
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE model.form_def SET name=$2, label=$3, fields=$4 WHERE id=$1::uuid`,
		formID, name, label, fieldsJSON)
	return err
}

func (s *Store) DeleteForm(ctx context.Context, formID string) error {
	// SYNC-01 cascade: drop form widgets referencing this form (ref_id is
	// bare TEXT, no FK — a dangling form widget rendered an empty error box).
	_, _ = s.pool.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE ref_id = $1`, formID)
	_, err := s.pool.Exec(ctx, `DELETE FROM model.form_def WHERE id=$1::uuid`, formID)
	return err
}

func (s *Store) CreateRecord(ctx context.Context, formID, userID string, data map[string]any) (*FormRecord, error) {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	var id string
	var createdAt, updatedAt time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO runtime.form_record (form_id, data, created_by)
		VALUES ($1::uuid, $2, $3::uuid)
		RETURNING id::text, created_at, updated_at
	`, formID, dataJSON, userID).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("create record: %w", err)
	}

	return &FormRecord{
		ID:        id,
		FormID:    formID,
		Data:      data,
		Status:    "draft",
		CreatedBy: userID,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

func (s *Store) ListRecords(ctx context.Context, formID string, limit int) ([]*FormRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, form_id::text, data, status::text, created_by::text, created_at, updated_at
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

func (s *Store) GetRecord(ctx context.Context, recordID string) (*FormRecord, error) {
	var r FormRecord
	var dataJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, form_id::text, data, status::text, created_by::text, created_at, updated_at
		FROM runtime.form_record WHERE id = $1::uuid
	`, recordID).Scan(&r.ID, &r.FormID, &dataJSON, &r.Status, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("record %s not found", recordID)
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

	tag, err := s.pool.Exec(ctx, `
		UPDATE runtime.form_record
		SET data = $2, status = $3::runtime.record_status, updated_at = now()
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

func (s *Store) DeleteRecord(ctx context.Context, recordID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM runtime.form_record WHERE id = $1::uuid`, recordID)
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
