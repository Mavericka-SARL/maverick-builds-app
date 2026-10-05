package importpkg

// Generic, name-based row resolution shared by every import entry point
// (currently the gateway HTTP handler; see its doc comment for why the gRPC
// ImportService's own ValidateImport isn't routed through this yet). A
// caller parses a file (CSV or native .xlsx — see xlsx.go) into a plain
// header + row-of-cells shape and hands it to ResolveRows, which is the one
// place that knows how to turn column headers into metric/dimension IDs, so
// a spreadsheet author never pastes an internal UUID: a column can be named
// after a metric ("salary") or a dimension ("employees", "months"), and two
// legacy sentinel columns ("metric_id", "value") are still accepted for
// backward compatibility with existing raw-UUID imports.
//
// Every referenced dimension member is validated as a leaf (no children) —
// structurally, not by dimension name, so a hierarchy parent (e.g. a year in
// a months dimension) is rejected for any demo's any dimension with no
// per-dimension configuration — and every value must be a number. Negative
// values are values (credits, losses, variances), as they are when typed into
// a cell or pulled by a connector; an import once refused them, a rule from
// the July 2026 salary demo. ResolveRows returns every row's outcome (staged or errored); the
// caller decides atomicity (see importUpload's "reject the whole workbook
// if any row is invalid" handling).

import (
	"context"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	importpkgv1 "github.com/mavericks-engine/mavericks/gen/go/importpkg/v1"
)

// RawRow is one data row from a parsed CSV or XLSX file, keyed by the
// original (untrimmed-case) header text.
// Querier is what name resolution reads through: a pool, or a transaction
// (the AI Developer's proposal check resolves a file against the model as
// the plan's earlier steps leave it).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type RawRow struct {
	RowNumber int
	Cells     map[string]string
}

var looksLikeUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type columnKind int

const (
	colIgnoredLegacyPair columnKind = iota // "metric_id" or "value": consumed together, not a metric/dim column itself
	colMetric
	colDimension
)

type columnMeta struct {
	kind     columnKind
	id       string // metric_id or dimension_id
	original string
}

// classifyColumns resolves each header cell to a metric, a dimension, or one
// of the two legacy sentinel columns. Returns an error naming the first
// unrecognized column — a header problem is a whole-file problem, reported
// before any row is examined.
func classifyColumns(ctx context.Context, pool Querier, modelID, revisionID string, header []string) (map[string]columnMeta, bool, bool, error) {
	cols := make(map[string]columnMeta, len(header))
	hasMetricIDCol, hasValueCol := false, false
	for _, raw := range header {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		switch lower {
		case "metric_id":
			cols[name] = columnMeta{kind: colIgnoredLegacyPair, original: name}
			hasMetricIDCol = true
			continue
		case "value":
			cols[name] = columnMeta{kind: colIgnoredLegacyPair, original: name}
			hasValueCol = true
			continue
		}

		var metricID string
		if err := pool.QueryRow(ctx,
			`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND lower(name)=$2 AND (revision_id=$3::uuid OR revision_id IS NULL) LIMIT 1`,
			modelID, lower, revisionID,
		).Scan(&metricID); err == nil {
			cols[name] = columnMeta{kind: colMetric, id: metricID, original: name}
			continue
		}

		var dimID string
		if err := pool.QueryRow(ctx,
			`SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND lower(name)=$2 AND (revision_id=$3::uuid OR revision_id IS NULL) LIMIT 1`,
			modelID, lower, revisionID,
		).Scan(&dimID); err == nil {
			cols[name] = columnMeta{kind: colDimension, id: dimID, original: name}
			continue
		}

		return nil, false, false, &unknownColumnError{name: name}
	}
	return cols, hasMetricIDCol, hasValueCol, nil
}

// unknownColumnError reports a header column matching neither a metric nor
// a dimension name — a whole-file problem, surfaced before any row is read.
type unknownColumnError struct{ name string }

func (e *unknownColumnError) Error() string {
	return "column header \"" + e.name + "\" matches no metric or dimension in this model"
}

// ResolveRows turns parsed (header, rows) into staged, write-ready rows plus
// a per-row/column error for anything that couldn't be resolved or failed
// validation: unknown metric/dimension reference, unknown member code, a
// non-leaf member (e.g. a hierarchy parent), a value that isn't a number.
// A row contributing to `errs` is never present in
// `staged` — the two slices partition the input exactly.
func ResolveRows(ctx context.Context, pool Querier, modelID, revisionID string, header []string, rows []RawRow) (staged []StagingRow, errs []*importpkgv1.ImportError, err error) {
	cols, hasMetricIDCol, hasValueCol, err := classifyColumns(ctx, pool, modelID, revisionID, header)
	if err != nil {
		return nil, nil, err
	}
	legacyPair := hasMetricIDCol && hasValueCol

	// Cache dimension_member lookups (dimID, code) -> (memberID, isLeaf) across
	// rows — an import file typically repeats the same member codes many times.
	type memberInfo struct {
		id     string
		code   string // the member's own code — differs from the file's value when it matched by label or month
		isLeaf bool
		calc   bool // a calculated member: computed, takes no input
		ok     bool
	}
	memberCache := map[string]memberInfo{}
	lookupMember := func(dimID, value string) memberInfo {
		key := dimID + "\x00" + value
		if v, ok := memberCache[key]; ok {
			return v
		}
		var info memberInfo
		var hasChildren bool
		const pick = `SELECT m.id::text, m.code, EXISTS(SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id),
			       NULLIF(btrim(m.formula),'') IS NOT NULL
			FROM model.dimension_member m `
		// A code first; then, only when exactly one member qualifies, the
		// member's label (sheets laid out for people carry "North America",
		// not NA), and on a monthly time dimension a month name ("Jan",
		// "January") for the one leaf period starting in that month.
		// Anything ambiguous stays unknown.
		found := pool.QueryRow(ctx, pick+`WHERE m.dimension_id=$1::uuid AND m.code=$2`, dimID, value).
			Scan(&info.id, &info.code, &hasChildren, &info.calc) == nil
		if !found {
			found = pool.QueryRow(ctx, pick+`WHERE m.dimension_id=$1::uuid AND lower(m.label)=lower($2)
				AND (SELECT count(*) FROM model.dimension_member o WHERE o.dimension_id=$1::uuid AND lower(o.label)=lower($2)) = 1`,
				dimID, value).Scan(&info.id, &info.code, &hasChildren, &info.calc) == nil
		}
		if month := monthNumber(value); !found && month > 0 {
			found = pool.QueryRow(ctx, pick+`JOIN model.dimension_def d ON d.id = m.dimension_id
				WHERE m.dimension_id=$1::uuid AND d.dimension_type='time' AND d.time_granularity='month'
				  AND m.period_start IS NOT NULL AND EXTRACT(MONTH FROM m.period_start) = $2
				  AND (SELECT count(*) FROM model.dimension_member o WHERE o.dimension_id=$1::uuid
				       AND o.period_start IS NOT NULL AND EXTRACT(MONTH FROM o.period_start) = $2) = 1`,
				dimID, month).Scan(&info.id, &info.code, &hasChildren, &info.calc) == nil
		}
		if found {
			info.isLeaf = !hasChildren
			info.ok = true
		}
		memberCache[key] = info
		return info
	}

	// picklistOf returns the dimension a pick-list metric's cells hold
	// members of ("" for any other metric), looked up once per metric.
	picklistDims := map[string]string{}
	picklistOf := func(metricID string) string {
		if d, ok := picklistDims[metricID]; ok {
			return d
		}
		var d string
		_ = pool.QueryRow(ctx, `SELECT COALESCE(picklist_dimension_id::text,'') FROM model.metric_def WHERE id=$1::uuid`, metricID).Scan(&d)
		picklistDims[metricID] = d
		return d
	}
	metricNameToID := map[string]string{} // populated lazily for legacy metric_id-as-name references
	resolveMetricRef := func(ref string) (id string, ok bool) {
		if looksLikeUUID.MatchString(ref) {
			// A UUID-shaped legacy metric_id reference must still belong to
			// this model/revision — accepting it as-is would let a caller
			// supply any metric UUID, including one from a different
			// tenant's model, and have it written under this model_id.
			var exists bool
			if err := pool.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM model.metric_def WHERE id=$1::uuid AND model_id=$2::uuid AND (revision_id=$3::uuid OR revision_id IS NULL))`,
				ref, modelID, revisionID,
			).Scan(&exists); err != nil || !exists {
				return "", false
			}
			return ref, true
		}
		if id, cached := metricNameToID[strings.ToLower(ref)]; cached {
			return id, id != ""
		}
		var metricID string
		found := pool.QueryRow(ctx,
			`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND lower(name)=$2 AND (revision_id=$3::uuid OR revision_id IS NULL) LIMIT 1`,
			modelID, strings.ToLower(ref), revisionID,
		).Scan(&metricID) == nil
		if found {
			metricNameToID[strings.ToLower(ref)] = metricID
			return metricID, true
		}
		metricNameToID[strings.ToLower(ref)] = ""
		return "", false
	}

	type valueEvent struct {
		metricRef string // raw reference (UUID, name, or already-resolved ID)
		valueRaw  string
		column    string
	}

	for _, row := range rows {
		var events []valueEvent
		if legacyPair {
			mref := strings.TrimSpace(row.Cells["metric_id"])
			vraw := strings.TrimSpace(row.Cells["value"])
			if mref != "" || vraw != "" {
				events = append(events, valueEvent{metricRef: mref, valueRaw: vraw, column: "value"})
			}
		}
		for colName, meta := range cols {
			if meta.kind != colMetric {
				continue
			}
			vraw := strings.TrimSpace(row.Cells[colName])
			if vraw == "" {
				continue
			}
			events = append(events, valueEvent{metricRef: meta.id, valueRaw: vraw, column: colName})
		}

		if len(events) == 0 {
			continue // a blank row, or a row with dimension cells but no value in any recognized column
		}

		// Dimension members for this row, resolved once and reused by every
		// value event on the row.
		dimMembers := map[string]string{}
		rowValid := true
		for colName, meta := range cols {
			if meta.kind != colDimension {
				continue
			}
			code := strings.TrimSpace(row.Cells[colName])
			if code == "" {
				continue
			}
			info := lookupMember(meta.id, code)
			if !info.ok {
				errs = append(errs, &importpkgv1.ImportError{
					RowNumber: int32(row.RowNumber), Column: colName,
					ErrorCode: "UNKNOWN_MEMBER", RawValue: code,
					Message: "\"" + code + "\" is not a member of dimension \"" + meta.original + "\"",
				})
				rowValid = false
				continue
			}
			if info.calc {
				errs = append(errs, &importpkgv1.ImportError{
					RowNumber: int32(row.RowNumber), Column: colName,
					ErrorCode: "CALCULATED_MEMBER", RawValue: code,
					Message: "\"" + code + "\" is a calculated member of \"" + meta.original + "\": its values are computed from the other members, so it takes no input",
				})
				rowValid = false
				continue
			}
			if !info.isLeaf {
				errs = append(errs, &importpkgv1.ImportError{
					RowNumber: int32(row.RowNumber), Column: colName,
					ErrorCode: "NOT_LEAF", RawValue: code,
					Message: "\"" + code + "\" is not a leaf member of \"" + meta.original + "\" (has child members)",
				})
				rowValid = false
				continue
			}
			dimMembers[meta.id] = info.code
		}
		if !rowValid {
			continue
		}

		for _, ev := range events {
			metricID, ok := resolveMetricRef(ev.metricRef)
			if !ok {
				errs = append(errs, &importpkgv1.ImportError{
					RowNumber: int32(row.RowNumber), Column: ev.column,
					ErrorCode: "UNKNOWN_METRIC", RawValue: ev.metricRef,
					Message: "\"" + ev.metricRef + "\" does not match any metric in this model",
				})
				continue
			}
			if dimID := picklistOf(metricID); dimID != "" {
				// A pick-list cell holds a member: the file names it by code
				// or label, and its key is stored.
				info := lookupMember(dimID, ev.valueRaw)
				if !info.ok || info.calc {
					errs = append(errs, &importpkgv1.ImportError{
						RowNumber: int32(row.RowNumber), Column: ev.column,
						ErrorCode: "UNKNOWN_MEMBER", RawValue: ev.valueRaw,
						Message: "\"" + ev.valueRaw + "\" is not a member of the dimension whose members this pick-list holds",
					})
					continue
				}
				memberCopy := make(map[string]string, len(dimMembers))
				for k, v := range dimMembers {
					memberCopy[k] = v
				}
				staged = append(staged, StagingRow{MetricID: metricID, DimMembers: memberCopy,
					Value: formula.PicklistKey(info.code), RowNumber: row.RowNumber})
				continue
			}
			val, perr := strconv.ParseFloat(ev.valueRaw, 64)
			if perr != nil {
				errs = append(errs, &importpkgv1.ImportError{
					RowNumber: int32(row.RowNumber), Column: ev.column,
					ErrorCode: "INVALID_NUMBER", RawValue: ev.valueRaw,
					Message: "cannot parse \"" + ev.valueRaw + "\" as a number",
				})
				continue
			}
			memberCopy := make(map[string]string, len(dimMembers))
			for k, v := range dimMembers {
				memberCopy[k] = v
			}
			staged = append(staged, StagingRow{
				MetricID:   metricID,
				DimMembers: memberCopy,
				Value:      val,
				RowNumber:  row.RowNumber,
			})
		}
	}

	return staged, errs, nil
}

// monthNumber reads an English month name or its three-letter abbreviation
// ("Jan", "january", "Sep", "Sept") as 1-12, and anything else as 0.
func monthNumber(s string) int {
	s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
	if s == "sept" {
		return 9
	}
	for i, name := range []string{"january", "february", "march", "april", "may", "june",
		"july", "august", "september", "october", "november", "december"} {
		if s == name || (len(s) == 3 && s == name[:3]) {
			return i + 1
		}
	}
	return 0
}
