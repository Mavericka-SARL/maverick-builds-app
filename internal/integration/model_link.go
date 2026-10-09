package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// A model link is a pull whose source is a grid of another model of the
// same tenant (Protocol "model"). Its records are that grid's values — one
// record per leaf intersection with a value, a field per dimension and per
// metric — and from there it is any other pull: the same mapping, the same
// commit into the link's own model, the same run history.
//
// Reading the source is not this package's: SourceReader is the gateway's
// own grid route, made as the developer the run acts for, so a link reads
// exactly what that developer sees and nothing the routes would refuse
// them. That is why the gateway, not the worker, claims these runs
// (Store.ClaimModelLink).

// SourceReader reads a model link's source grid as the developer runBy.
// It refuses, with a *SourceError, a source that is not a model of the
// link's tenant, a developer who no longer builds both models, and a grid
// missing from the source model's active revision.
type SourceReader interface {
	ReadModelSource(ctx context.Context, def *Definition, runBy string) (*SourceTable, error)
}

// SourceTable is a source grid read as text records.
type SourceTable struct {
	Header []string
	Rows   [][]string
	// Model and Revision name what was read, for the run's history.
	Model    string
	Revision string
	Warnings []string
}

// SourceError is a refusal to read a source, with the run error code it
// is recorded under.
type SourceError struct {
	Code string
	Msg  string
}

func (e *SourceError) Error() string { return e.Msg }

// validateModel checks a model link: pull only, a source model and grid,
// and no sign-in — a link reads as the developer the run acts for.
func (c *Config) validateModel() error {
	m := c.Model
	if m == nil {
		return fmt.Errorf("model settings are required for the model protocol")
	}
	if c.SFTP != nil {
		return fmt.Errorf("sftp settings apply to the sftp protocol only")
	}
	if c.Direction != DirectionPull {
		return fmt.Errorf("a model link reads another model (direction pull)")
	}
	// Every run reads the source's values as they stand: added to the
	// target's (incremental), a second run would double them.
	if c.ImportMode != ModeReplace && c.ImportMode != ModeFullReload {
		return fmt.Errorf("a model link imports with replace or full_reload — each run reads the source's whole values, which incremental would add again")
	}
	if _, err := uuid.Parse(m.ModelID); err != nil {
		return fmt.Errorf("model.model_id must name the source model")
	}
	if strings.TrimSpace(m.Grid) == "" {
		return fmt.Errorf("model.grid must name the source grid")
	}
	for _, name := range m.Metrics {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("model.metrics: a metric name is empty")
		}
	}
	for dim, codes := range m.Filters {
		if strings.TrimSpace(dim) == "" || len(codes) == 0 {
			return fmt.Errorf("model.filters: each filter names a dimension and at least one member")
		}
	}
	switch m.MemberDisplay {
	case "", "code", "label":
	default:
		return fmt.Errorf("model.member_display must be code or label")
	}
	if c.Auth.Type != "" && c.Auth.Type != "none" {
		return fmt.Errorf("a model link signs in as the developer who runs it — auth must be none")
	}
	return nil
}

// runAllowed is the switch check every run passes before it reads
// anything. A real run (not a test or a dry run) of a switched-off
// connector does not run, and a scheduled one of a draft does not either:
// the scheduler skips them and the gateway refuses to queue a manual run of
// either, but both can change before the run is claimed. A model link
// switched off on its source side reads nothing at all, not even for a
// test: that switch is the source model's say over its data.
func runAllowed(run *Run, def *Definition) error {
	if def.Config != nil && def.Config.Protocol == ProtocolModel && !def.SourceEnabled {
		return fmt.Errorf("switched off on the source model's side")
	}
	if run.DryRun || run.TriggerType == "test" {
		return nil
	}
	if !def.Enabled {
		return fmt.Errorf("the integration is switched off")
	}
	if run.TriggerType == "schedule" && def.Status != "active" {
		return fmt.Errorf("the integration is a draft")
	}
	return nil
}

// executeModelPull reads the source grid through rn.Source and maps and
// commits its records through the same tail as every other pull.
func (rn *Runner) executeModelPull(ctx context.Context, run *Run, def *Definition, res RunResult) RunResult {
	cfg := def.Config
	if rn.Source == nil {
		res.ErrorCode, res.Message = ErrCodeInternal, "a model link is run by the gateway, not by this worker"
		return res
	}
	if run.RunBy == "" {
		// Nobody to read as: a schedule enabled before anyone was recorded.
		res.ErrorCode, res.Message = ErrCodeAuth, "the run acts for no developer — switch the schedule on again"
		return res
	}
	res.Meta["source_grid"] = cfg.Model.Grid

	start := rn.now()
	tbl, err := rn.Source.ReadModelSource(ctx, def, run.RunBy)
	res.Requests = 1
	durMS := int(rn.now().Sub(start).Milliseconds())
	target := "model:" + cfg.Model.Grid
	if err != nil {
		code := ErrCodeInternal
		var se *SourceError
		if errors.As(err, &se) {
			code = se.Code
		}
		rn.Store.RecordAttempt(ctx, run.ID, 1, 1, "READ", target, 0, durMS, code, sanitizeMsg(err.Error()))
		res.ErrorCode, res.Message = code, err.Error()
		return res
	}
	rn.Store.RecordAttempt(ctx, run.ID, 1, 1, "READ", target, 0, durMS, "", "")
	res.Meta["source_model"] = tbl.Model
	res.Meta["source_revision"] = tbl.Revision
	if len(tbl.Warnings) > 0 {
		res.Meta["source_warnings"] = strings.Join(tbl.Warnings, "; ")
	}

	records := tableRecords(tbl.Header, tbl.Rows)
	maxRecords := cfg.Limits.MaxRecords
	if maxRecords <= 0 {
		maxRecords = 100_000
	}
	if len(records) > maxRecords {
		records = records[:maxRecords]
		res.Meta["stopped"] = "max_records"
	}
	res.Pages = 1
	res.RecordsRead = len(records)

	if run.TriggerType == "test" {
		attachFilePreview(&res, tbl.Header, records)
	}

	failThreshold := cfg.Limits.FailureThreshold
	if failThreshold <= 0 {
		failThreshold = 100
	}
	h, rows, recordErrs := MapPullRecords(cfg, records, 0)
	if len(recordErrs) > failThreshold {
		res.ErrorCode, res.Message = ErrCodeInvalidData, fmt.Sprintf("%d record(s) failed mapping (threshold %d)", len(recordErrs), failThreshold)
		res.RecordsSkipped = len(recordErrs)
		return res
	}
	return rn.finishPull(ctx, run, def, h, rows, recordErrs, res)
}

// tableRecords keys each row by its column name, the shape every other
// pull's records have.
func tableRecords(header []string, rows [][]string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		rec := make(map[string]any, len(header))
		for j, h := range header {
			if j < len(r) {
				rec[h] = r[j]
			} else {
				rec[h] = ""
			}
		}
		out = append(out, rec)
	}
	return out
}
