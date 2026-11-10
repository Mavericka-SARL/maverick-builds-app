package writeguard

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Input rules: the values an input metric accepts (model.metric_def.
// input_rules, migration 173) — no value below a minimum, none above a
// maximum, whole numbers only. The database states them once
// (model.input_value_accepted, model.input_rule_refusal) and refuses a stored
// value they refuse on whichever path it came (runtime.input_rules_guard);
// the paths that answer per cell or per row ask the same functions first, so
// a batch names the cell and an import the row.

// InputRuleCode is the SQLSTATE the guard refuses a value with.
const InputRuleCode = "MV422"

// CodeInputRule is the code a refused cell or import row carries.
const CodeInputRule = "INPUT_RULE"

// ValueRefused is a value an input metric's rules refuse; Message names the
// metric, what it accepts, the value and its cell.
type ValueRefused struct{ Message string }

func (e *ValueRefused) Error() string { return e.Message }

// AsValueRefused answers the refusal err is or carries — the guard's, from
// any statement that stored values — or nil.
func AsValueRefused(err error) *ValueRefused {
	var vr *ValueRefused
	if errors.As(err, &vr) {
		return vr
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == InputRuleCode {
		return &ValueRefused{Message: pgErr.Message}
	}
	return nil
}

// ValueCell is a number written into an input metric's cell.
type ValueCell struct {
	MetricID string
	DimCodes map[string]string
	Value    float64
}

// InputRuleRefusals answers, by index in cells, the sentence refusing each
// value its metric's rules refuse; none when every value is accepted. A text
// or pick-list value is not a cell's number: leave it out.
func InputRuleRefusals(ctx context.Context, q Querier, cells []ValueCell) (map[int]string, error) {
	if len(cells) == 0 {
		return nil, nil
	}
	// Only the cells of metrics with rules are sent: an import of 100,000
	// values into metrics without any asks one short question.
	seen := map[string]bool{}
	var metricIDs []string
	for _, c := range cells {
		if !seen[c.MetricID] {
			seen[c.MetricID] = true
			metricIDs = append(metricIDs, c.MetricID)
		}
	}
	ruled, err := q.Query(ctx, `SELECT id::text FROM model.metric_def WHERE id::text = ANY($1::text[]) AND input_rules <> '{}'::jsonb`, metricIDs)
	if err != nil {
		return nil, err
	}
	withRules := map[string]bool{}
	for ruled.Next() {
		var id string
		if err := ruled.Scan(&id); err != nil {
			ruled.Close()
			return nil, err
		}
		withRules[id] = true
	}
	ruled.Close()
	if err := ruled.Err(); err != nil {
		return nil, err
	}
	if len(withRules) == 0 {
		return nil, nil
	}
	var at []int // index in cells of each cell sent
	var ids, dims []string
	var values []float64
	for i, c := range cells {
		if !withRules[c.MetricID] {
			continue
		}
		d := "{}"
		if len(c.DimCodes) > 0 {
			b, err := json.Marshal(c.DimCodes)
			if err != nil {
				return nil, err
			}
			d = string(b)
		}
		at = append(at, i)
		ids, values, dims = append(ids, c.MetricID), append(values, c.Value), append(dims, d)
	}
	rows, err := q.Query(ctx, `
		SELECT c.i - 1, r.refusal
		FROM unnest($1::uuid[], $2::float8[], $3::jsonb[]) WITH ORDINALITY AS c(metric_id, v, dims, i)
		JOIN model.metric_def m ON m.id = c.metric_id AND m.input_rules <> '{}'::jsonb
		CROSS JOIN LATERAL (SELECT model.input_rule_refusal(m, c.v::numeric, c.dims) AS refusal) r
		WHERE r.refusal IS NOT NULL
		ORDER BY c.i`, ids, values, dims)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out map[int]string
	for rows.Next() {
		var i int
		var refusal string
		if err := rows.Scan(&i, &refusal); err != nil {
			return nil, err
		}
		if out == nil {
			out = map[int]string{}
		}
		out[at[i]] = refusal
	}
	return out, rows.Err()
}

// SkipInputRules runs op with the guard told that the values db's
// transaction stores are carried, not entered — a revision copy, a model
// import, a workflow's copy on approval, a restructure, a records posting —
// so a rule added after they were stored does not refuse them. db must be a
// transaction: on a pool the setting would reach no statement of op.
func SkipInputRules(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, op func() error) error {
	if _, err := db.Exec(ctx, `SELECT set_config('mvx.skip_input_rules', 'on', true)`); err != nil {
		return err
	}
	opErr := op()
	if _, err := db.Exec(ctx, `SELECT set_config('mvx.skip_input_rules', '', true)`); err != nil && opErr == nil {
		return err
	}
	return opErr
}
