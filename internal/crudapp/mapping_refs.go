package crudapp

// What a table-to-metric mapping (model.form_metric_mapping) may name: only
// its own model's table, grid, input metric and dimensions, in its own
// revision, and fields its table has. The developer API stored whatever ids
// a body named, so a mapping could name another tenant's table and post —
// or, by a backfill, read — that tenant's records.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// MappingRefs are the references a mapping holds.
type MappingRefs struct {
	FormID         string
	GridID         string // "" for none
	TargetMetricID string
	SourceField    string
	// DimensionMappings: dimension id → field name.
	DimensionMappings map[string]string
}

// MappingRefError is a reference a mapping may not hold; its message says
// which, without saying whether the id exists elsewhere.
type MappingRefError struct{ msg string }

func (e *MappingRefError) Error() string { return e.msg }

// IsMappingRefError reports whether err is a MappingRefError.
func IsMappingRefError(err error) bool {
	var m *MappingRefError
	return errors.As(err, &m)
}

func refErr(format string, args ...any) error {
	return &MappingRefError{msg: fmt.Sprintf(format, args...)}
}

// inRevision is the condition that row alias t belongs to model $1 and, when
// revision $2 is given, to that revision or to none (revision-global).
const inRevision = `t.model_id = $1::uuid AND ($2 = '' OR t.revision_id IS NULL OR t.revision_id::text = $2)`

// CheckMappingRefs says what a mapping of model modelID in revision
// revisionID ("" for a revision-global one) names that is not its own: the
// table, the grid, the target metric (an input metric) and every dimension
// must be the model's, in that revision; the source field and every mapped
// field must be fields of the table. nil when all are.
func CheckMappingRefs(ctx context.Context, db dbx.DB, modelID, revisionID string, m MappingRefs) error {
	found := func(table, id, extra string) (bool, error) {
		if uuid.Validate(id) != nil {
			return false, nil
		}
		var ok bool
		err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` t WHERE t.id = $3::uuid AND `+inRevision+extra+`)`,
			modelID, revisionID, id).Scan(&ok)
		return ok, err
	}

	if uuid.Validate(m.FormID) != nil {
		return refErr("form_id names no table of this model's revision")
	}
	var fieldsJSON []byte
	err := db.QueryRow(ctx, `SELECT t.fields FROM model.form_def t WHERE t.id = $3::uuid AND `+inRevision,
		modelID, revisionID, m.FormID).Scan(&fieldsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return refErr("form_id names no table of this model's revision")
	}
	if err != nil {
		return err
	}
	var fields []FormField
	_ = json.Unmarshal(fieldsJSON, &fields)
	has := func(name string) bool {
		return slices.ContainsFunc(fields, func(f FormField) bool { return f.Name == name })
	}

	if ok, err := found("model.metric_def", m.TargetMetricID, " AND t.is_input"); err != nil {
		return err
	} else if !ok {
		return refErr("target_metric_id names no input metric of this model's revision")
	}
	if m.GridID != "" {
		if ok, err := found("model.grid_def", m.GridID, ""); err != nil {
			return err
		} else if !ok {
			return refErr("grid_id names no grid of this model's revision")
		}
	}
	if m.SourceField != "" && !has(m.SourceField) {
		return refErr("source_field %q is no field of the table", m.SourceField)
	}
	dims := make([]string, 0, len(m.DimensionMappings))
	for d := range m.DimensionMappings {
		dims = append(dims, d)
	}
	sort.Strings(dims)
	for _, dimID := range dims {
		if ok, err := found("model.dimension_def", dimID, ""); err != nil {
			return err
		} else if !ok {
			return refErr("dimension_mappings names %q, which is no dimension of this model's revision", dimID)
		}
		if field := m.DimensionMappings[dimID]; !has(field) {
			return refErr("dimension_mappings maps a dimension to %q, which is no field of the table", field)
		}
	}
	return nil
}
