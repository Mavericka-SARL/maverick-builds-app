package metricformula

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// InputRules are the values an input metric's cells accept
// (model.metric_def.input_rules, migration 173): none below Min, none above
// Max, whole numbers only when Whole — in the metric's stored units (a
// percentage metric stores 15 for 15%). Every path that enters values
// refuses one they refuse (writeguard.InputRuleRefusals and the database's
// guard); a clear is always accepted.
type InputRules struct {
	Min   *float64 `json:"min,omitempty"`
	Max   *float64 `json:"max,omitempty"`
	Whole bool     `json:"whole,omitempty"`
}

// CodeInvalidInputRules refuses input rules the engine could not apply.
const CodeInvalidInputRules = "INVALID_INPUT_RULES"

// InputRuleFormats are the formats whose cells hold a number the rules
// check ("" is number).
var InputRuleFormats = []string{"", "number", "percentage", "currency"}

// CheckInputRules validates the input rules of a metric that is saved as
// isInput with format, and returns them as JSON to store: {} for none (raw
// empty, null or {}). Rules are for a numeric input metric: a calculated
// metric takes no input, and a text, date, boolean or pick-list cell holds
// no amount to bound.
func CheckInputRules(raw json.RawMessage, isInput bool, format string) (json.RawMessage, error) {
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" {
		return json.RawMessage("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r InputRules
	if err := dec.Decode(&r); err != nil {
		return nil, invalidCode(CodeInvalidInputRules, `input_rules is {"min": n, "max": n, "whole": true}, any of them: %v`, err)
	}
	if r.Min == nil && r.Max == nil && !r.Whole {
		return json.RawMessage("{}"), nil
	}
	if !isInput {
		return nil, invalidCode(CodeInvalidInputRules, "input_rules are for input metrics: a calculated metric takes no input")
	}
	if !slices.Contains(InputRuleFormats, format) {
		return nil, invalidCode(CodeInvalidInputRules, "input_rules bound a number: a %s metric's cells hold none (rules apply to number, percentage and currency metrics)", format)
	}
	if r.Min != nil && r.Max != nil && *r.Min > *r.Max {
		return nil, invalidCode(CodeInvalidInputRules, "input_rules: min %g is above max %g, so no value would be accepted", *r.Min, *r.Max)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("input_rules: %w", err)
	}
	return b, nil
}
