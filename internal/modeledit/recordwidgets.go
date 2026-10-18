package modeledit

// The settings of the dashboard widgets that show a table's records: the
// records widget ("form") and the board ("board"). Both may keep fixed
// filters, which the viewer cannot remove ("record_filters", the records
// query's filters, relative dates included: "overdue tasks" is
// [{"field":"due","op":"before_today"},{"field":"state","op":"ne","value":"Done"}]).
// A records widget may also choose its columns and order ("record_columns",
// "record_sort"). A board lays the records out as cards in columns
// ("board"): the columns are the members of a dimension field — any
// dimension — or the choices of a select field, in their order.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// BoardSettings are a board widget's "board" props.
type BoardSettings struct {
	// ColumnField is the dimension or select field whose members or choices
	// are the board's columns; moving a card sets it.
	ColumnField string `json:"column_field"`
	// TitleField titles each card (the table's first text field when "").
	TitleField string `json:"title_field,omitempty"`
	// CardFields are the fields each card shows under its title.
	CardFields []string `json:"card_fields,omitempty"`
	// SumField is a number field totalled per column ("" counts only).
	SumField string `json:"sum_field,omitempty"`
	// ColorProperty is the column dimension's member property naming each
	// column's colour (a CSS colour: #2f80ed, teal).
	ColorProperty string `json:"color_property,omitempty"`
	// HideEmpty leaves out the columns no card is in.
	HideEmpty bool `json:"hide_empty,omitempty"`
}

// RecordSort is a records widget's fixed order.
type RecordSort struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

// CheckRecordWidgetProps checks the record settings in props of a widget of
// widgetType ("form" or "board") over the table formID: the fields they
// name are the table's, of the kind each setting needs, and the filters are
// filters the records query answers. Other widget types pass.
func CheckRecordWidgetProps(ctx context.Context, db DB, widgetType, formID string, props json.RawMessage) error {
	if widgetType != "form" && widgetType != "board" {
		return nil
	}
	if len(props) == 0 || string(props) == "null" {
		if widgetType == "board" {
			return fmt.Errorf(`a board needs widget_props.board.column_field: the dimension or select field whose members are its columns`)
		}
		return nil
	}
	var p struct {
		Board   *BoardSettings   `json:"board"`
		Filters []crudapp.Filter `json:"record_filters"`
		Columns []string         `json:"record_columns"`
		Sort    *RecordSort      `json:"record_sort"`
	}
	if err := json.Unmarshal(props, &p); err != nil {
		return fmt.Errorf("widget_props: %w", err)
	}
	form, err := crudapp.NewStoreOn(db).GetForm(ctx, formID)
	if err != nil {
		return fmt.Errorf("the widget's table: %w", err)
	}
	if len(p.Filters) > 0 {
		if err := crudapp.CheckFilters(form, p.Filters); err != nil {
			return fmt.Errorf("record_filters: %w", err)
		}
	}
	for _, c := range p.Columns {
		if !form.HasColumn(c) {
			return fmt.Errorf("record_columns names %q, which is not a field of %s (its fields: %s)", c, form.Label, fieldNames(form))
		}
	}
	if p.Sort != nil && !form.HasColumn(p.Sort.Field) {
		return fmt.Errorf("record_sort names %q, which is not a field of %s", p.Sort.Field, form.Label)
	}
	if widgetType == "form" {
		if p.Board != nil {
			return fmt.Errorf(`"board" settings belong to a board widget, not a records widget`)
		}
		return nil
	}
	if p.Board == nil || p.Board.ColumnField == "" {
		return fmt.Errorf(`a board needs widget_props.board.column_field: the dimension or select field whose members are its columns`)
	}
	b := p.Board
	col := form.Field(b.ColumnField)
	if col == nil || (col.Type != "dimension" && col.Type != "select") || (col.Type == "dimension" && col.DimensionID == "") {
		return fmt.Errorf("board.column_field %q is not a dimension or select field of %s (its fields: %s)", b.ColumnField, form.Label, fieldNames(form))
	}
	if b.TitleField != "" && form.Field(b.TitleField) == nil {
		return fmt.Errorf("board.title_field %q is not a field of %s", b.TitleField, form.Label)
	}
	for _, c := range b.CardFields {
		if !form.HasColumn(c) {
			return fmt.Errorf("board.card_fields names %q, which is not a field of %s (its fields: %s)", c, form.Label, fieldNames(form))
		}
	}
	if b.SumField != "" {
		if f := form.Field(b.SumField); f == nil || f.Type != "number" {
			return fmt.Errorf("board.sum_field %q is not a number field of %s", b.SumField, form.Label)
		}
	}
	if b.ColorProperty != "" {
		if col.Type != "dimension" {
			return fmt.Errorf("board.color_property reads a member property: the column field must be a dimension field")
		}
		var ok bool
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM model.dimension_property WHERE dimension_id = $1::uuid AND name = $2)`,
			col.DimensionID, b.ColorProperty).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("board.color_property %q is not a property of the column field's dimension", b.ColorProperty)
		}
	}
	return nil
}

func fieldNames(f *crudapp.FormDef) string {
	names := make([]string, 0, len(f.Fields))
	for _, fd := range f.Fields {
		names = append(names, fd.Name)
	}
	return strings.Join(names, ", ")
}
