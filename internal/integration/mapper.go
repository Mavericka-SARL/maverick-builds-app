package integration

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The mapper turns extracted response records into the header+rows shape
// importpkg.ResolveRows consumes (Pull), or Mavericks rows into template
// row-contexts (Push). Transforms are the closed declarative set — applied
// in order, never user code.

// applyTransforms runs a field's transform chain over one raw value.
func applyTransforms(v any, transforms []Transform) (string, error) {
	// Everything flows as string between transforms, like a spreadsheet cell.
	s := valueToString(v)
	for _, t := range transforms {
		switch t.Kind {
		case TransformNone:
		case TransformTrim:
			s = strings.TrimSpace(s)
		case TransformToString:
			// already a string
		case TransformToNumber:
			if s == "" {
				continue
			}
			f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				return "", fmt.Errorf("%q is not a number", s)
			}
			s = strconv.FormatFloat(f, 'f', -1, 64)
		case TransformToBoolean:
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true", "1", "yes", "y":
				s = "true"
			case "false", "0", "no", "n", "":
				s = "false"
			default:
				return "", fmt.Errorf("%q is not a boolean", s)
			}
		case TransformToDate, TransformDateFormat:
			if s == "" {
				continue
			}
			parsed, err := parseAnyDate(strings.TrimSpace(s))
			if err != nil {
				return "", err
			}
			layout := t.Value
			switch layout {
			case "", "iso":
				layout = "2006-01-02"
			}
			s = parsed.Format(layout)
		case TransformDefault:
			if strings.TrimSpace(s) == "" {
				s = t.Value
			}
		case TransformLookup:
			if mapped, ok := t.Lookup[s]; ok {
				s = mapped
			} else if fallback, ok := t.Lookup["*"]; ok {
				s = fallback
			}
			// No entry and no "*" fallback: value passes through unchanged —
			// dropping it silently would hide data; downstream validation
			// (unknown member, bad number) reports it per-record instead.
		}
	}
	return s, nil
}

func valueToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

var dateLayouts = []string{
	time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05",
	"2006-01-02", "01/02/2006", "02.01.2006", "2006/01/02",
}

func parseAnyDate(s string) (time.Time, error) {
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a recognized date", s)
}

// RecordError is one per-record mapping failure (dry-run report material).
type RecordError struct {
	Record  int    `json:"record"` // 1-based within the run
	Field   string `json:"field"`
	Message string `json:"message"`
}

// MapPullRecords produces the header + rows importpkg.ResolveRows takes.
//
// Wide shape: each FieldMap row is one output column (Target = metric,
// dimension, or "member_*"/"property:*" for dimension targets).
// Long shape: MetricNameSource/ValueSource name the metric column and value;
// dimension FieldMaps still apply per record.
func MapPullRecords(cfg *Config, records []map[string]any, recordOffset int) (header []string, rows [][]string, errs []RecordError) {
	fields := cfg.Mapping.Fields
	long := cfg.Mapping.Shape == ShapeLong

	header = make([]string, 0, len(fields)+2)
	for _, f := range fields {
		header = append(header, f.Target)
	}
	var metricCol int
	if long {
		header = append(header, "__metric__", "__value__")
		metricCol = len(header) - 2
	}

	compiled := make([][]pathStep, len(fields))
	for i, f := range fields {
		steps, err := ParsePath(f.Source)
		if err != nil {
			errs = append(errs, RecordError{Record: 0, Field: f.Source, Message: err.Error()})
			return nil, nil, errs
		}
		compiled[i] = steps
	}
	var metricSteps, valueSteps []pathStep
	if long {
		metricSteps, _ = ParsePath(cfg.Mapping.MetricNameSource)
		valueSteps, _ = ParsePath(cfg.Mapping.ValueSource)
	}

	for ri, rec := range records {
		row := make([]string, len(header))
		ok := true
		for fi, f := range fields {
			v, _ := LookupPath(rec, compiled[fi])
			s, terr := applyTransforms(v, f.Transforms)
			if terr != nil {
				errs = append(errs, RecordError{Record: recordOffset + ri + 1, Field: f.Target, Message: terr.Error()})
				ok = false
				break
			}
			row[fi] = s
		}
		if !ok {
			continue
		}
		if long {
			mv, _ := LookupPath(rec, metricSteps)
			vv, _ := LookupPath(rec, valueSteps)
			row[metricCol] = valueToString(mv)
			row[metricCol+1] = valueToString(vv)
			if row[metricCol] == "" {
				errs = append(errs, RecordError{Record: recordOffset + ri + 1, Field: cfg.Mapping.MetricNameSource, Message: "metric name is empty"})
				continue
			}
		}
		rows = append(rows, row)
	}
	return header, rows, errs
}

// LongToWide regroups long-shape rows into ResolveRows' wide header/cells:
// one output row per unique dimension-combo, metric columns from the data.
func LongToWide(header []string, rows [][]string) (wideHeader []string, wideRows [][]string) {
	metricCol := len(header) - 2
	dimHeader := header[:metricCol]

	metricSet := map[string]int{}
	var metricOrder []string
	type comboRow struct {
		dims   []string
		values map[string]string
	}
	var combos []*comboRow
	comboIdx := map[string]*comboRow{}
	for _, r := range rows {
		key := strings.Join(r[:metricCol], "\x00")
		c, ok := comboIdx[key]
		if !ok {
			c = &comboRow{dims: r[:metricCol], values: map[string]string{}}
			comboIdx[key] = c
			combos = append(combos, c)
		}
		m := r[metricCol]
		if _, seen := metricSet[m]; !seen {
			metricSet[m] = len(metricOrder)
			metricOrder = append(metricOrder, m)
		}
		c.values[m] = r[metricCol+1]
	}
	wideHeader = append(append([]string{}, dimHeader...), metricOrder...)
	for _, c := range combos {
		row := append([]string{}, c.dims...)
		for _, m := range metricOrder {
			row = append(row, c.values[m])
		}
		wideRows = append(wideRows, row)
	}
	return wideHeader, wideRows
}

// MapPushRecord renders one Mavericks row into the {{row.*}} template values.
func MapPushRecord(cfg *Config, source map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(cfg.Mapping.Fields))
	for _, f := range cfg.Mapping.Fields {
		v := source[f.Source]
		s, err := applyTransforms(v, f.Transforms)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", f.Source, err)
		}
		out[f.Source] = s
		_ = f.Target // Target names the outbound JSON property in body templates
	}
	return out, nil
}

// BuildPushBatchBody renders the batched JSON body: {batch_property: [ {target: value...} ]}.
func BuildPushBatchBody(cfg *Config, rows []map[string]string) (string, error) {
	prop := cfg.Mapping.BatchProperty
	if prop == "" {
		prop = "items"
	}
	var b strings.Builder
	b.WriteString(`{"` + jsonEscape(prop) + `":[`)
	for i, row := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('{')
		for j, f := range cfg.Mapping.Fields {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"` + jsonEscape(f.Target) + `":"` + jsonEscape(row[f.Source]) + `"`)
		}
		b.WriteByte('}')
	}
	b.WriteString(`]}`)
	return b.String(), nil
}

func jsonEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return r.Replace(s)
}
