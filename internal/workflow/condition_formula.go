package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/query"
)

// A condition step may test the model instead of the instance context:
// {"formula": "ABS(company_var_pct) > variance_threshold"} — "variance over
// threshold → another round". The formula is the model's formula language;
// a name in it is a metric of the workflow's revision, read at the instance's
// point (each "Dimension member" context variable pins its dimension, every
// other dimension is at its total), else a context variable (a number when it
// parses as one, else text). True or non-zero takes the "true" route; a
// formula that cannot be evaluated leaves the step for a person, as an
// unevaluable {left, operator, right} condition does.

// conditionFormula is the formula of a condition step's condition, or "".
func conditionFormula(raw json.RawMessage) string {
	var c struct {
		Formula string `json:"formula"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Formula), "="))
}

// checkConditionFormula is ValidateDef's structural check of one.
func checkConditionFormula(label, text string) string {
	if _, err := formula.Analyze(text); err != nil {
		return fmt.Sprintf("Condition step %q: its formula does not parse: %v", label, err)
	}
	return ""
}

// CheckConditionNames resolves every name of def's formula conditions in its
// workflow's revision: a metric or a context variable of the definition. It
// returns one line per name that is neither.
func (s *Store) CheckConditionNames(ctx context.Context, def *WorkflowDefFull) []string {
	var steps []map[string]any
	if json.Unmarshal(def.Steps, &steps) != nil {
		return nil
	}
	modelID, revisionID := s.defModelRevision(ctx, def.ID, def.ApplicationID)
	contextKeys := map[string]bool{}
	var schema []struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(def.ContextSchema, &schema)
	for _, v := range schema {
		contextKeys[strings.ToLower(v.Key)] = true
	}
	var errs []string
	for _, st := range steps {
		raw, _ := json.Marshal(st["condition"])
		text := conditionFormula(raw)
		if text == "" {
			continue
		}
		an, err := formula.Analyze(text)
		if err != nil {
			continue // ValidateDef reports it
		}
		label, _ := st["name"].(string)
		for _, ref := range an.References {
			if contextKeys[strings.ToLower(ref.Name)] {
				continue
			}
			var isMetric bool
			_ = s.db.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM model.metric_def WHERE model_id::text=$1 AND revision_id::text=$2 AND lower(name)=lower($3))`,
				modelID, revisionID, ref.Name).Scan(&isMetric)
			if !isMetric {
				errs = append(errs, fmt.Sprintf("Condition step %q: its formula names %q, which is no metric of the workflow's revision and no context variable", label, ref.Name))
			}
		}
	}
	return errs
}

// defModelRevision is the model and revision a definition's conditions read:
// its own revision, else its application's model's active one.
func (s *Store) defModelRevision(ctx context.Context, defID, appID string) (modelID, revisionID string) {
	_ = s.db.QueryRow(ctx, `
		SELECT r.model_id::text, r.id::text
		FROM workflow.workflow_def wd
		JOIN model.revision r ON r.id = wd.revision_id
		WHERE wd.id::text = $1`, defID).Scan(&modelID, &revisionID)
	if revisionID != "" {
		return modelID, revisionID
	}
	_ = s.db.QueryRow(ctx, `
		SELECT m.id::text, COALESCE(m.active_revision_id::text,'')
		FROM core.model m WHERE m.application_id::text = $1
		ORDER BY m.created_at LIMIT 1`, appID).Scan(&modelID, &revisionID)
	return modelID, revisionID
}

// evalConditionFormula evaluates a formula condition for an instance:
// (result, true), or (false, false) with the reason when it cannot.
func (s *Store) evalConditionFormula(ctx context.Context, instanceID, text string) (bool, bool, string) {
	node, err := formula.Parse(text)
	if err != nil {
		return false, false, fmt.Sprintf("the formula does not parse: %v", err)
	}
	an, err := formula.Analyze(text)
	if err != nil {
		return false, false, err.Error()
	}
	var defID, appID string
	var ctxJSON, schemaJSON []byte
	if err := s.db.QueryRow(ctx, `
		SELECT wd.id::text, wd.application_id::text, wi.context, COALESCE(wi.context_schema_snapshot, wd.context_schema, '[]'::jsonb)
		FROM workflow.workflow_instance wi JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.id = $1::uuid`, instanceID).Scan(&defID, &appID, &ctxJSON, &schemaJSON); err != nil {
		return false, false, "the instance was not found"
	}
	ctxVars := map[string]string{}
	_ = json.Unmarshal(ctxJSON, &ctxVars)
	modelID, revisionID := s.defModelRevision(ctx, defID, appID)
	// A started instance may name the revision it works on; it must be
	// one of the same application's models.
	if rev := ctxVars["revision_id"]; rev != "" && rev != revisionID {
		var m string
		if s.db.QueryRow(ctx, `
			SELECT r.model_id::text FROM model.revision r JOIN core.model m ON m.id = r.model_id
			WHERE r.id::text = $1 AND m.application_id::text = $2`, rev, appID).Scan(&m) == nil {
			modelID, revisionID = m, rev
		}
	}
	if s.pool == nil || modelID == "" || revisionID == "" {
		return false, false, "the workflow's model could not be read"
	}

	// Each "Dimension member" variable pins its dimension (by name in the
	// revision read, so a definition built on another revision still pins).
	var schema []struct {
		Key         string `json:"key"`
		DataType    string `json:"data_type"`
		DimensionID string `json:"dimension_id"`
	}
	_ = json.Unmarshal(schemaJSON, &schema)
	point := map[string]string{}
	for _, v := range schema {
		code := ctxVars[v.Key]
		if v.DataType != "Dimension member" || v.DimensionID == "" || code == "" {
			continue
		}
		var dimID string
		if s.db.QueryRow(ctx, `
			SELECT d.id::text FROM model.dimension_def d JOIN model.dimension_def o ON lower(o.name) = lower(d.name)
			WHERE o.id::text = $1 AND d.model_id::text = $2 AND d.revision_id::text = $3`, v.DimensionID, modelID, revisionID).Scan(&dimID) == nil {
			point[dimID] = code
		}
	}

	names := make([]string, 0, len(an.References))
	for _, ref := range an.References {
		names = append(names, ref.Name)
	}
	values, err := query.NewChartResolver(s.pool).MetricValuesAt(ctx, modelID, revisionID, names, point)
	if err != nil {
		return false, false, fmt.Sprintf("the model could not be read: %v", err)
	}
	vars := make(map[string]formula.Value, len(names))
	for _, name := range names {
		if v, ok := values[name]; ok {
			vars[strings.ToUpper(name)] = v
			continue
		}
		raw, ok := lookupFold(ctxVars, name)
		if !ok {
			return false, false, fmt.Sprintf("%q is no metric and no context variable", name)
		}
		if n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
			vars[strings.ToUpper(name)] = formula.NumberVal(n)
		} else {
			vars[strings.ToUpper(name)] = formula.StringVal(raw)
		}
	}
	v := formula.EvalNode(&formula.EvalContext{Vars: vars}, node)
	if v.IsError() {
		return false, false, fmt.Sprintf("it evaluates to %s: %s", v.Err().Code, v.Err().Message)
	}
	switch v.Kind() {
	case formula.KindBool, formula.KindNumber:
		return v.Bool(), true, ""
	case formula.KindString, formula.KindBlank, formula.KindError:
	}
	return false, false, "it gives no true or false"
}

func lookupFold(m map[string]string, key string) (string, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}
