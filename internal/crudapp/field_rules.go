package crudapp

// Field rules (2026-11-20, asked for a help desk): opt-in options of a
// table's fields that tie what a save may do to what the record holds —
// the moves a status may make (Transitions), values in which the record is
// read-only (LocksIn), a field required in some states only (RequiredWhen),
// a date the server sets when the record reaches a state (StampWhen), and
// people as values (the person field type). A table naming none of them
// saves exactly as before: every function here returns at once for it.
//
// The checks run inside recordpost's RecordValueProblems, which every
// route that saves a record goes through — the console's form and board,
// the API and the chat connector's update_record, file imports, scheduled
// connectors and the AI's saves.

import (
	"fmt"
	"slices"
	"strings"
)

// FieldPerson is the type of a field naming a person: its value is the id
// of a user who reaches the table's application.
const FieldPerson = "person"

// ValueText is v as the text a rule compares: trimmed, "" for nothing.
func ValueText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// ConditionMet reports whether data meets c (as a posting's row filter
// condition is met).
func ConditionMet(c *RowCondition, data map[string]any) bool {
	return c != nil && RowFilterMatches([]RowCondition{*c}, data)
}

// HasTransitions reports whether the field's moves are ruled.
func (f FormField) HasTransitions() bool { return len(f.Transitions) > 0 }

// MovesFrom lists the values the field may move to from value from, for a
// person holds says holds a list of roles (holds(nil) is always true): the
// To of every transition whose From names from and whose roles the person
// holds, in the order the transitions name them, without from itself. A
// field without transitions answers nil: any value may follow.
func (f FormField) MovesFrom(from string, holds func(roles []string) bool) []string {
	var out []string
	for _, t := range f.Transitions {
		if !slices.ContainsFunc(t.From, func(v string) bool { return sameText(v, from) }) {
			continue
		}
		if len(t.Roles) > 0 && (holds == nil || !holds(t.Roles)) {
			continue
		}
		for _, to := range t.To {
			if !sameText(to, from) && !slices.ContainsFunc(out, func(v string) bool { return sameText(v, to) }) {
				out = append(out, to)
			}
		}
	}
	return out
}

// moveRoles are the roles named by the transitions that move the field
// from from to to; nil when one of them names none (anyone may), or when
// none moves it so (then ok is false).
func (f FormField) moveRoles(from, to string) (roles []string, ok bool) {
	for _, t := range f.Transitions {
		if !slices.ContainsFunc(t.From, func(v string) bool { return sameText(v, from) }) ||
			!slices.ContainsFunc(t.To, func(v string) bool { return sameText(v, to) }) {
			continue
		}
		if len(t.Roles) == 0 {
			return nil, true
		}
		ok = true
		for _, r := range t.Roles {
			if !slices.Contains(roles, r) {
				roles = append(roles, r)
			}
		}
	}
	return roles, ok
}

// TransitionRoles lists every role the table's transitions name, so the
// roles a person holds among them can be read once.
func (form *FormDef) TransitionRoles() []string {
	var out []string
	for _, f := range form.Fields {
		for _, t := range f.Transitions {
			for _, r := range t.Roles {
				if !slices.Contains(out, r) {
					out = append(out, r)
				}
			}
		}
	}
	return out
}

// LockingField is the field whose value makes a record holding data
// read-only (one of its LocksIn), nil when the record is not locked.
func (form *FormDef) LockingField(data map[string]any) *FormField {
	if form == nil {
		return nil
	}
	for i := range form.Fields {
		f := &form.Fields[i]
		if len(f.LocksIn) == 0 {
			continue
		}
		v := ValueText(data[f.Name])
		if v != "" && slices.ContainsFunc(f.LocksIn, func(l string) bool { return sameText(l, v) }) {
			return f
		}
	}
	return nil
}

// HasFieldRules reports whether any field of the table has a rule a save
// is checked or changed by here.
func (form *FormDef) HasFieldRules() bool {
	if form == nil {
		return false
	}
	return slices.ContainsFunc(form.Fields, func(f FormField) bool {
		return len(f.Transitions) > 0 || len(f.LocksIn) > 0 || f.StampWhen != nil || f.RequiredWhen != nil
	})
}

// RuleProblem is a save refused by a field's rule.
type RuleProblem struct {
	Field   string
	Message string
}

// ValueLabel names a field's value in a rule's sentence (a dimension
// field's member by its label); nil writes values as they are stored.
type ValueLabel func(f FormField, v string) string

func (l ValueLabel) of(f FormField, v string) string {
	if l == nil {
		return v
	}
	return l(f, v)
}

func (l ValueLabel) list(f FormField, vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = l.of(f, v)
	}
	return out
}

// RuleProblems checks a save that leaves a record holding data, from
// current (nil for a new record), against the table's locks and
// transitions. holds says whether the person saving holds one of a list of
// roles (an administrator holds every one); label names values in the
// sentences. A new record may start in any value; a record that held no
// value may take any.
func RuleProblems(form *FormDef, current, data map[string]any, holds func(roles []string) bool, label ValueLabel) []RuleProblem {
	if form == nil || current == nil {
		return nil
	}
	var out []RuleProblem
	if lock := form.LockingField(current); lock != nil {
		for _, f := range form.Fields {
			if f.Name == lock.Name || f.Type == FieldAutoCode || f.Type == FieldStatus || f.StampWhen != nil {
				continue
			}
			if !sameText(ValueText(current[f.Name]), ValueText(data[f.Name])) {
				out = append(out, RuleProblem{Field: f.Name, Message: fmt.Sprintf(
					"this record is read-only while its %s is %s: only its %s may change",
					fieldLabel(*lock), label.of(*lock, ValueText(current[lock.Name])), fieldLabel(*lock))})
				break
			}
		}
	}
	for _, f := range form.Fields {
		if !f.HasTransitions() {
			continue
		}
		from, to := ValueText(current[f.Name]), ValueText(data[f.Name])
		if from == "" || sameText(from, to) {
			continue
		}
		roles, ok := f.moveRoles(from, to)
		switch {
		case !ok:
			msg := fmt.Sprintf("%s cannot move from %s to %s", fieldLabel(f), label.of(f, from), orNothing(label.of(f, to)))
			if next := f.MovesFrom(from, holds); len(next) > 0 {
				msg += fmt.Sprintf(" (from %s it moves to %s)", label.of(f, from), strings.Join(label.list(f, next), ", "))
			}
			out = append(out, RuleProblem{Field: f.Name, Message: msg})
		case len(roles) > 0 && (holds == nil || !holds(roles)):
			out = append(out, RuleProblem{Field: f.Name, Message: fmt.Sprintf(
				"only %s move %s from %s to %s", strings.Join(roles, ", "), fieldLabel(f), label.of(f, from), label.of(f, to))})
		}
	}
	return out
}

// RequiredWhenMissing is the fields that a record holding data must fill
// in by their RequiredWhen and does not, each with its sentence.
func RequiredWhenMissing(form *FormDef, data map[string]any, label ValueLabel) []RuleProblem {
	if form == nil {
		return nil
	}
	var out []RuleProblem
	for _, f := range form.Fields {
		if f.RequiredWhen == nil || ValueText(data[f.Name]) != "" || !ConditionMet(f.RequiredWhen, data) {
			continue
		}
		out = append(out, RuleProblem{Field: f.Name, Message: fmt.Sprintf("%s is required when %s",
			fieldLabel(f), describeCondition(form, f.RequiredWhen, label))})
	}
	return out
}

// ApplyStamps sets each StampWhen date field of a record saved from
// current (nil for a new record) to data: today when the save brings the
// record to meet its condition; otherwise the value it held. A new record
// arriving with a date keeps it.
func ApplyStamps(form *FormDef, current, data map[string]any, today string) {
	if form == nil {
		return
	}
	for _, f := range form.Fields {
		if f.StampWhen == nil {
			continue
		}
		met := ConditionMet(f.StampWhen, data)
		if current == nil {
			if met && ValueText(data[f.Name]) == "" {
				data[f.Name] = today
			}
			continue
		}
		switch {
		case met && !ConditionMet(f.StampWhen, current):
			data[f.Name] = today
		default:
			if v, ok := current[f.Name]; ok && v != nil {
				data[f.Name] = v
			} else {
				delete(data, f.Name)
			}
		}
	}
}

// validateRuleOptions checks f's rule options against its type and the
// table's other fields.
func validateRuleOptions(f FormField, fields []FormField) error {
	byName := func(n string) *FormField {
		for i := range fields {
			if fields[i].Name == n {
				return &fields[i]
			}
		}
		return nil
	}
	choiceField := f.Type == "select" || f.Type == "dimension"
	checkValues := func(what string, values []string) error {
		if len(values) == 0 {
			return fmt.Errorf("%s names no value", what)
		}
		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("%s names an empty value", what)
			}
			if f.Type == "select" && len(f.Options) > 0 && !slices.Contains(f.Options, v) {
				return fmt.Errorf("%s names %q, which is not one of its choices", what, v)
			}
		}
		return nil
	}
	if len(f.Transitions) > 0 {
		if !choiceField {
			return fmt.Errorf("transitions are a select or dimension field's")
		}
		for i, t := range f.Transitions {
			if err := checkValues(fmt.Sprintf("transition %d's from", i+1), t.From); err != nil {
				return err
			}
			if err := checkValues(fmt.Sprintf("transition %d's to", i+1), t.To); err != nil {
				return err
			}
			for _, r := range t.Roles {
				if strings.TrimSpace(r) == "" {
					return fmt.Errorf("transition %d names an empty role", i+1)
				}
			}
		}
	}
	if len(f.LocksIn) > 0 {
		if !choiceField {
			return fmt.Errorf("locks_in is a select or dimension field's")
		}
		if err := checkValues("locks_in", f.LocksIn); err != nil {
			return err
		}
		for _, o := range fields {
			if o.Name != f.Name && len(o.LocksIn) > 0 {
				return fmt.Errorf("a table has one field with locks_in; %q has it too", o.Name)
			}
		}
	}
	checkCondition := func(what string, c *RowCondition) error {
		other := byName(c.Field)
		switch {
		case other == nil:
			return fmt.Errorf("%s names %q, which is not a field of the table", what, c.Field)
		case other.Name == f.Name:
			return fmt.Errorf("%s names the field itself", what)
		case other.Type == FieldStatus:
			return fmt.Errorf("%s names the status field, which a workflow sets: name a field a person fills in", what)
		case (len(c.Is) == 0) == (len(c.IsNot) == 0):
			return fmt.Errorf("%s needs \"is\" or \"is_not\": the values %s holds (or must not hold)", what, c.Field)
		}
		if other.Type == "select" && len(other.Options) > 0 {
			for _, v := range append(append([]string{}, c.Is...), c.IsNot...) {
				if !slices.Contains(other.Options, v) {
					return fmt.Errorf("%s: %q is not one of the choices of %s", what, v, c.Field)
				}
			}
		}
		return nil
	}
	if f.RequiredWhen != nil {
		if f.Type == FieldAutoCode || f.Type == FieldStatus || f.StampWhen != nil {
			return fmt.Errorf("required_when is for a field the person fills in")
		}
		if err := checkCondition("required_when", f.RequiredWhen); err != nil {
			return err
		}
	}
	if f.StampWhen != nil {
		switch {
		case f.Type != "date":
			return fmt.Errorf("stamp_when is a date field's")
		case f.Default != nil:
			return fmt.Errorf("a stamped date has no default: the server sets it")
		case f.Required || f.RequiredWith != "":
			return fmt.Errorf("a stamped date is not required: the server sets it")
		}
		if err := checkCondition("stamp_when", f.StampWhen); err != nil {
			return err
		}
	}
	if f.ActiveFlag {
		if f.Type != "boolean" {
			return fmt.Errorf("active_flag is a boolean field's")
		}
		for _, o := range fields {
			if o.Name != f.Name && o.ActiveFlag {
				return fmt.Errorf("a table has one active_flag field; %q has it too", o.Name)
			}
		}
	}
	if len(f.Roles) > 0 && f.Type != FieldPerson {
		return fmt.Errorf("roles are a person field's")
	}
	if f.Notify && f.Type != FieldPerson && f.Type != "dimension" {
		return fmt.Errorf("notify is a person field's, or a dimension field's naming records that name a person")
	}
	for _, r := range f.Roles {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("roles names an empty role")
		}
	}
	if f.Type == FieldPerson && f.Default != nil {
		return fmt.Errorf("a person field has no default")
	}
	return nil
}

func sameText(a, b string) bool { return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) }

func fieldLabel(f FormField) string { return firstNonEmpty(f.Label, f.Name) }

func orNothing(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

// describeCondition is c as a sentence's end: "Status is Resolved or
// Closed".
func describeCondition(form *FormDef, c *RowCondition, values ValueLabel) string {
	label, of := c.Field, FormField{Name: c.Field}
	if o := form.Field(c.Field); o != nil {
		label, of = fieldLabel(*o), *o
	}
	if len(c.Is) > 0 {
		return fmt.Sprintf("%s is %s", label, strings.Join(values.list(of, c.Is), " or "))
	}
	return fmt.Sprintf("%s is not %s", label, strings.Join(values.list(of, c.IsNot), " or "))
}
