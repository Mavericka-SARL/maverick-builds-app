package aiassistant

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
)

// Typed values in bulk, and files the assistant writes (2026-10-15): asked
// to clear test data, the assistant could only save a full-reload
// integration that never ran, and it could not hand the developer a file.
//
//   - clear_input_values: a grid's (or one metric's) typed values, under
//     chosen members or all, through the developer's own Clear values route.
//   - fill_sample_values: a grid's input metrics filled with sample values on
//     the server, or, with as_file, the same rows attached as a CSV.
//   - create_csv_file (immediate, no confirmation): a CSV the assistant
//     writes, attached to the chat — downloadable, and importable with
//     import_file_data.

// maxWrittenCSVRows bounds a CSV the assistant writes row by row; a larger
// dataset is drawn on the server (fill_sample_values, as_file).
const maxWrittenCSVRows = 5000

// clearInputValues is clear_input_values.
func (e *WriteExecutor) clearInputValues(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		GridID    string          `json:"grid_id"`
		MetricID  string          `json:"metric_id"`
		MetricIDs []string        `json:"metric_ids"`
		Members   json.RawMessage `json:"members"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	kind, id := "grid", strings.TrimSpace(p.GridID)
	if id == "" {
		kind, id = "metric", strings.TrimSpace(p.MetricID)
	}
	if id == "" {
		return "", "", fmt.Errorf("grid_id (list_grids) or metric_id is required")
	}
	if err := e.inModel(ctx, kind, id); err != nil {
		return "", "", err
	}
	if e.hooks.ClearValues == nil {
		return "", "", fmt.Errorf("values are cleared through the gateway, which is not available here")
	}
	body, _ := json.Marshal(map[string]any{"metric_ids": p.MetricIDs, "members": p.Members})
	out, err := e.hooks.ClearValues(ctx, kind, id, body)
	if err != nil {
		return "", "", err
	}
	return out, id, nil
}

// fillSampleValues is fill_sample_values.
func (e *WriteExecutor) fillSampleValues(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		GridID    string          `json:"grid_id"`
		Metrics   json.RawMessage `json:"metrics"`
		Members   json.RawMessage `json:"members"`
		OnlyEmpty bool            `json:"only_empty"`
		Seed      uint64          `json:"seed"`
		AsFile    bool            `json:"as_file"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	id := strings.TrimSpace(p.GridID)
	if id == "" {
		return "", "", fmt.Errorf("grid_id is required (list_grids)")
	}
	if err := e.inModel(ctx, "grid", id); err != nil {
		return "", "", err
	}
	if e.hooks.FillSample == nil {
		return "", "", fmt.Errorf("sample values are drawn through the gateway, which is not available here")
	}
	body, _ := json.Marshal(map[string]any{"metrics": p.Metrics, "members": p.Members, "only_empty": p.OnlyEmpty, "seed": p.Seed})
	out, err := e.hooks.FillSample(ctx, id, body, p.AsFile)
	if err != nil {
		return "", "", err
	}
	return out, id, nil
}

// inModel refuses a grid or metric that is not this model's.
func (e *WriteExecutor) inModel(ctx context.Context, kind, id string) error {
	table := map[string]string{"grid": "model.grid_def", "metric": "model.metric_def"}[kind]
	var ok bool
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id::text = $1 AND model_id = $2::uuid)`, id, e.modelID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s %s is not in this model (list_grids / list_metrics)", kind, id)
	}
	return nil
}

// createCSVFile is create_csv_file: a CSV the assistant writes, attached to
// the chat.
func (e *ToolExecutor) createCSVFile(ctx context.Context, raw json.RawMessage) (string, error) {
	var p struct {
		Filename string   `json:"filename"`
		Columns  []string `json:"columns"`
		Rows     [][]any  `json:"rows"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", fmt.Errorf("invalid params: %w", err)
	}
	name := strings.TrimSpace(p.Filename)
	if name == "" {
		return "", fmt.Errorf("filename is required")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".csv") {
		name += ".csv"
	}
	if len(p.Columns) == 0 {
		return "", fmt.Errorf("columns are required: the file's header")
	}
	if len(p.Rows) > maxWrittenCSVRows {
		return "", fmt.Errorf("%d rows is more than %d: draw a larger dataset on the server with fill_sample_values and as_file", len(p.Rows), maxWrittenCSVRows)
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(p.Columns)
	for i, r := range p.Rows {
		if len(r) != len(p.Columns) {
			return "", fmt.Errorf("row %d has %d cells, the header %d", i+1, len(r), len(p.Columns))
		}
		rec := make([]string, len(r))
		for j, c := range r {
			if c != nil {
				rec[j] = fmt.Sprint(c)
			}
		}
		_ = w.Write(rec)
	}
	w.Flush()
	if e.hooks.AttachCSV == nil {
		return "", fmt.Errorf("files are attached through the gateway, which is not available here")
	}
	return e.hooks.AttachCSV(ctx, name, buf.Bytes())
}
