package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/internal/query"
)

// A context variable may carry a default (2026-11-21): a formula in the
// model's formula language, worked out when a run starts without the value —
// a scheduled run, which brings nothing, a button, a record, or a person who
// leaves the field as the start dialog filled it. `set_first_week` (a date
// setting) gives a period variable the planning week the plan starts on,
// `TODAY()` today, `"Base"` a fixed member. A name in it is a metric of the
// workflow's revision — read at the run's point as a condition reads it, at
// the members the variables before it pin, every other dimension at its
// total — or a variable earlier in the schema. The result is written as the
// variable's type wants it: a date as yyyy-mm-dd (for a period variable, the
// period holding that date), a number as written, a pick-list's member code
// as it is. A default reads the model as the workflow's conditions do, not
// as the person starting it: name only values every starter may see.
//
// No default, or a value given, behaves as before.

// ErrContextDefault: a variable's default could not be worked out.
var ErrContextDefault = errors.New("a default could not be worked out")

// defaultFormula is a variable's default formula, "" when it has none.
func defaultFormula(cv startContextVarDef) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cv.Default), "="))
}

// hasValue reports whether m holds a non-blank value for key, in any case.
func hasValue(m map[string]string, key string) bool {
	v, ok := lookupFold(m, key)
	return ok && strings.TrimSpace(v) != ""
}

// applyContextDefaults fills, in the schema's order, each variable that has
// a default and no value in result. A RACI-resolved variable is the
// caller's grant, never a default.
func (s *Store) applyContextDefaults(ctx context.Context, defID, appID string, vars []startContextVarDef, result map[string]string) error {
	for _, cv := range vars {
		text := defaultFormula(cv)
		if text == "" || cv.SourceHint == "raci_responsible" || hasValue(result, cv.Key) {
			continue
		}
		val, err := s.contextDefaultValue(ctx, defID, appID, vars, cv, text, result)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrContextDefault, cv.Key, err)
		}
		if val != "" {
			result[cv.Key] = val
		}
	}
	return nil
}

// StartDefaults works out the defaults of a workflow's variables as a start
// with nothing given would: the start dialog shows them filled in. A default
// that cannot be worked out is in errs with the reason, and leaves its field
// empty.
func (s *Store) StartDefaults(ctx context.Context, defID string) (values, errs map[string]string, err error) {
	def, err := s.GetWorkflowDefFull(ctx, defID)
	if err != nil {
		return nil, nil, err
	}
	var vars []startContextVarDef
	_ = json.Unmarshal(def.ContextSchema, &vars)
	values, errs = map[string]string{}, map[string]string{}
	for _, cv := range vars {
		text := defaultFormula(cv)
		if text == "" || cv.SourceHint == "raci_responsible" {
			continue
		}
		val, derr := s.contextDefaultValue(ctx, def.ID, def.ApplicationID, vars, cv, text, values)
		if derr != nil {
			errs[cv.Key] = derr.Error()
			continue
		}
		if val != "" {
			values[cv.Key] = val
		}
	}
	return values, errs, nil
}

// contextDefaultValue evaluates cv's default text against the model and the
// variables known so far, written as cv's type wants it.
func (s *Store) contextDefaultValue(ctx context.Context, defID, appID string, vars []startContextVarDef, cv startContextVarDef, text string, known map[string]string) (string, error) {
	node, err := formula.Parse(text)
	if err != nil {
		return "", fmt.Errorf("the default does not parse: %v", err)
	}
	an, err := formula.Analyze(text)
	if err != nil {
		return "", err
	}
	modelID, revisionID := s.modelRevisionFor(ctx, defID, appID, known)
	if s.pool == nil || modelID == "" || revisionID == "" {
		return "", errors.New("the workflow's model could not be read")
	}
	point := s.memberPoint(ctx, vars, known, modelID, revisionID)
	names := make([]string, 0, len(an.References))
	for _, ref := range an.References {
		names = append(names, ref.Name)
	}
	resolver := query.NewChartResolver(s.pool)
	metricValues, err := resolver.MetricValuesAt(ctx, modelID, revisionID, names, point)
	if err != nil {
		return "", fmt.Errorf("the model could not be read: %v", err)
	}
	vals := make(map[string]formula.Value, len(names))
	for _, name := range names {
		if v, ok := metricValues[name]; ok {
			vals[strings.ToUpper(name)] = v
			continue
		}
		raw, ok := lookupFold(known, name)
		if !ok {
			return "", fmt.Errorf("%q is no metric and no variable given before this one", name)
		}
		if n, perr := strconv.ParseFloat(strings.TrimSpace(raw), 64); perr == nil {
			vals[strings.ToUpper(name)] = formula.NumberVal(n)
		} else {
			vals[strings.ToUpper(name)] = formula.StringVal(raw)
		}
	}
	v, err := resolver.EvalAt(ctx, modelID, revisionID, node, vals, point)
	if err != nil {
		return "", fmt.Errorf("the model could not be read: %v", err)
	}
	if v.IsError() {
		return "", fmt.Errorf("it evaluates to %s: %s", v.Err().Code, v.Err().Message)
	}
	return s.defaultString(ctx, cv, v), nil
}

// defaultString writes a default's value as cv's type wants it: a date as
// yyyy-mm-dd (a period variable then takes the period holding it), a number
// as written, TRUE/FALSE as true/false, a text or a member code as it is.
func (s *Store) defaultString(ctx context.Context, cv startContextVarDef, v formula.Value) string {
	switch v.Kind() {
	case formula.KindBlank, formula.KindError:
		return ""
	case formula.KindString:
		return strings.TrimSpace(v.String())
	case formula.KindBool:
		return strconv.FormatBool(v.Bool())
	case formula.KindNumber:
	}
	n, _ := v.Number()
	switch cv.DataType {
	case "Date":
		return formula.SerialDate(n)
	case "Dimension member":
		var dated bool
		_ = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM model.dimension_member WHERE dimension_id::text = $1 AND period_start IS NOT NULL)`,
			cv.DimensionID).Scan(&dated)
		if dated {
			if code, ok := s.periodHolding(ctx, cv.DimensionID, formula.SerialDate(n)); ok {
				return code
			}
			return formula.SerialDate(n)
		}
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

// memberPoint is the point a run's formulas read the model at: each
// "Dimension member" variable with a value pins its dimension in the
// revision read — the copy of the same lineage, so a definition built on
// another revision still pins, and so does a dimension renamed since (by
// name, a rename lost the member, 2026-11-09); else one of the same name.
// A date given for a period variable pins the period holding it.
func (s *Store) memberPoint(ctx context.Context, vars []startContextVarDef, ctxVars map[string]string, modelID, revisionID string) map[string]string {
	point := map[string]string{}
	for _, v := range vars {
		code, _ := lookupFold(ctxVars, v.Key)
		code = strings.TrimSpace(code)
		if v.DataType != "Dimension member" || v.DimensionID == "" || code == "" {
			continue
		}
		if c, ok := s.periodHolding(ctx, v.DimensionID, code); ok {
			code = c
		}
		var dimID string
		if s.db.QueryRow(ctx, `
			SELECT d.id::text FROM model.dimension_def d
			JOIN model.dimension_def o ON o.lineage_id = d.lineage_id OR lower(o.name) = lower(d.name)
			WHERE o.id::text = $1 AND d.model_id::text = $2 AND d.revision_id::text = $3
			ORDER BY (o.lineage_id = d.lineage_id) DESC LIMIT 1`, v.DimensionID, modelID, revisionID).Scan(&dimID) == nil {
			point[dimID] = code
		}
	}
	return point
}

// checkContextDefaults is CheckConditionNames for the variables' defaults:
// each parses, names a metric of the workflow's revision that holds a
// number, a date or a member (a text metric reads as nothing here) or a
// variable before it, and a member variable's default — when it can be
// worked out now — names a member of its dimension.
func (s *Store) checkContextDefaults(ctx context.Context, def *WorkflowDefFull, modelID, revisionID string) []string {
	var vars []startContextVarDef
	if json.Unmarshal(def.ContextSchema, &vars) != nil {
		return nil
	}
	var errs []string
	before := map[string]bool{}
	for _, cv := range vars {
		text := defaultFormula(cv)
		if text != "" {
			errs = append(errs, s.checkContextDefault(ctx, def, vars, cv, text, before, modelID, revisionID)...)
		}
		before[strings.ToLower(cv.Key)] = true
	}
	return errs
}

func (s *Store) checkContextDefault(ctx context.Context, def *WorkflowDefFull, vars []startContextVarDef, cv startContextVarDef, text string, before map[string]bool, modelID, revisionID string) []string {
	label := fmt.Sprintf("Variable %q", cv.Key)
	if cv.SourceHint == "raci_responsible" {
		return []string{label + ": it is taken from the starter's RACI scope, so it has no default"}
	}
	an, err := formula.Analyze(text)
	if err != nil {
		return []string{fmt.Sprintf("%s: its default does not parse: %v", label, err)}
	}
	var errs []string
	for _, ref := range an.References {
		var exists bool
		var format string
		_ = s.db.QueryRow(ctx, `
			SELECT true, COALESCE(format,'') FROM model.metric_def
			WHERE model_id::text=$1 AND revision_id::text=$2 AND lower(name)=lower($3) LIMIT 1`,
			modelID, revisionID, ref.Name).Scan(&exists, &format)
		switch {
		case exists && format == "text":
			errs = append(errs, fmt.Sprintf("%s: its default reads %s, a text metric, which a default cannot read; use a pick-list, a number or a date", label, ref.Name))
		case exists:
		case before[strings.ToLower(ref.Name)]:
		default:
			errs = append(errs, fmt.Sprintf("%s: its default names %q, which is no metric of the workflow's revision and no variable before it", label, ref.Name))
		}
	}
	if len(errs) > 0 || cv.DataType != "Dimension member" || cv.DimensionID == "" {
		return errs
	}
	// A member default that can be worked out now must name a member.
	val, err := s.contextDefaultValue(ctx, def.ID, def.ApplicationID, vars, cv, text, map[string]string{})
	if err != nil || val == "" {
		return nil
	}
	var member bool
	_ = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM model.dimension_member WHERE dimension_id::text = $1 AND code = $2)`,
		cv.DimensionID, val).Scan(&member)
	if !member {
		return []string{fmt.Sprintf("%s: its default gives %q, which is no member of its dimension", label, val)}
	}
	return nil
}

// ScheduleContextGaps names the variables a scheduled run of defID cannot
// fill: required, with no default — a schedule brings no values — or taken
// from a RACI scope, which the scheduler has none of.
func (s *Store) ScheduleContextGaps(ctx context.Context, defID string) ([]string, error) {
	var raw json.RawMessage
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(context_schema, '[]'::jsonb) FROM workflow.workflow_def WHERE id::text = $1`, defID).Scan(&raw); err != nil {
		return nil, err
	}
	return ScheduleGaps(raw), nil
}

// ScheduleGaps is ScheduleContextGaps for a context schema as stored.
func ScheduleGaps(schema json.RawMessage) []string {
	var vars []startContextVarDef
	_ = json.Unmarshal(schema, &vars)
	var gaps []string
	for _, cv := range vars {
		if !cv.Required {
			continue
		}
		if cv.SourceHint == "raci_responsible" || defaultFormula(cv) == "" {
			gaps = append(gaps, cv.Key)
		}
	}
	return gaps
}

// refuseScheduleGaps refuses a schedule rule whose workflow (by id, else by
// name as fireRule finds it) has variables a scheduled run cannot fill: the
// rule would fail at every fire.
func (s *Store) refuseScheduleGaps(ctx context.Context, appID, revisionID, defID, workflowName string) error {
	if defID == "" {
		if s.db.QueryRow(ctx, `
			SELECT id::text FROM workflow.workflow_def
			WHERE application_id = $1::uuid AND name = $2 AND status != 'archived'
			  AND ($3 = '' OR revision_id IS NULL OR revision_id::text = $3)
			ORDER BY created_at DESC LIMIT 1`, appID, workflowName, revisionID).Scan(&defID) != nil {
			return nil // no workflow yet: fireRule reports it
		}
	}
	gaps, err := s.ScheduleContextGaps(ctx, defID)
	if err != nil || len(gaps) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the workflow needs %s when it starts, and a schedule brings no values: give %s a default (the workflow's Context), or make %s optional",
		ErrRuleScheduleInvalid, strings.Join(gaps, ", "), pronounFor(gaps), pronounFor(gaps))
}

// pronounFor is "it" for one variable, "them" for several.
func pronounFor(keys []string) string {
	if len(keys) == 1 {
		return "it"
	}
	return "them"
}

// notifyScheduleFailure tells the application's business admins and the
// tenant's developers that a scheduled fire failed (2026-11-21, owner: they
// must be told; a schedule has no person to tell, notifyStartFailure's
// starter). Once per run of failures: a schedule failing every Monday tells
// them on the first Monday, and again after it has run once.
func (s *Store) notifyScheduleFailure(ctx context.Context, rule AutomationRule, execID string, fireErr error) {
	var prev string
	_ = s.db.QueryRow(ctx, `
		SELECT status::text FROM workflow.execution
		WHERE rule_id = $1::uuid AND id <> $2::uuid AND scheduled_for IS NOT NULL
		ORDER BY started_at DESC, id DESC LIMIT 1`, rule.ID, execID).Scan(&prev)
	if prev == "failed" {
		return
	}
	rows, err := s.db.Query(ctx, `
		WITH app AS (
		    SELECT a.workspace_id, w.customer_id FROM core.application a JOIN core.workspace w ON w.id = a.workspace_id
		    WHERE a.id = $1::uuid)
		SELECT DISTINCT u.id::text
		FROM identity."user" u JOIN identity.role_assignment ra ON ra.user_id = u.id CROSS JOIN app
		WHERE COALESCE(u.keycloak_sub, '') <> 'system-scheduler'
		  AND ((ra.role = 'business_admin' AND ra.workspace_id = app.workspace_id)
		    OR (ra.role = 'developer' AND (ra.workspace_id IN (SELECT id FROM core.workspace WHERE customer_id = app.customer_id)
		                                OR (ra.workspace_id IS NULL AND u.customer_id = app.customer_id))))`, rule.ApplicationID)
	if err != nil {
		return
	}
	var users []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			users = append(users, id)
		}
	}
	rows.Close()
	vars := map[string]string{
		"subject": "A scheduled trigger could not start its workflow",
		"message": fmt.Sprintf("%s: %s. You are told again only after it has run successfully.", rule.Name, fireErr.Error()),
	}
	ns := notification.NewStoreOn(s.db)
	for _, u := range users {
		_, _ = ns.Notify(ctx, u, "workflow_step_notification", vars, "automation_rule", rule.ID)
	}
}
