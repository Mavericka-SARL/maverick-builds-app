package importpkg

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// Sample values: a grid's input metrics filled across its leaf
// combinations, on the server — test data for a model, as many cells as the
// grid has, without a file or the AI Developer writing every row.

// SampleSpec is how one metric's values are drawn: Value for a fixed value,
// otherwise uniformly from Min to Max rounded to Decimals. A pick-list metric
// draws one of its options instead.
type SampleSpec struct {
	Min      float64  `json:"min"`
	Max      float64  `json:"max"`
	Decimals int      `json:"decimals"`
	Value    *float64 `json:"value,omitempty"`
}

// SampleRequest is what to fill.
type SampleRequest struct {
	ModelID, RevisionID, UserID string
	// DimensionIDs are the grid's dimensions.
	DimensionIDs []string
	// Metrics are the input metrics to fill and how.
	Metrics map[string]SampleSpec
	// Members limits the fill as ClearRequest.Members does.
	Members map[string][]string
	// OnlyEmpty fills only cells holding no value.
	OnlyEmpty bool
	// Seed makes the values repeatable; the same request draws the same.
	Seed uint64
	// MaxCells bounds the cells one request fills.
	MaxCells int
}

// SampleResult is the cells drawn (as staging rows, ready to commit) and
// the combinations left out because the caller may not write them.
type SampleResult struct {
	Rows     []StagingRow
	Withheld int
}

// GenerateSample draws req's values. It refuses a metric the caller may not
// write, a calculated or text metric, and more cells than req.MaxCells.
func GenerateSample(ctx context.Context, pool *pgxpool.Pool, req SampleRequest) (*SampleResult, error) {
	if len(req.Metrics) == 0 {
		return nil, ErrNothingToClear
	}
	ids := make([]string, 0, len(req.Metrics))
	for id := range req.Metrics {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var plans []samplePlan
	for _, id := range ids {
		var input bool
		var format, picklistDim string
		if err := pool.QueryRow(ctx, `
			SELECT is_input, format, COALESCE(picklist_dimension_id::text, '') FROM model.metric_def
			WHERE id::text = $1 AND model_id = $2::uuid AND (revision_id = $3::uuid OR revision_id IS NULL)`,
			id, req.ModelID, req.RevisionID).Scan(&input, &format, &picklistDim); err != nil {
			return nil, fmt.Errorf("metric %s is not in this model's revision", id)
		}
		if !input {
			return nil, fmt.Errorf("metric %s is calculated: it takes no typed values", id)
		}
		if format == "text" {
			return nil, fmt.Errorf("metric %s holds text: sample values are numbers and pick-list options", id)
		}
		access, err := writeguard.MetricAccess(ctx, pool, req.UserID, id)
		if err != nil {
			return nil, err
		}
		if access == "hidden" || access == "read" {
			return nil, fmt.Errorf("%w: a metric to fill is outside your access scope", ErrWriteDenied)
		}
		p := samplePlan{id: id, spec: req.Metrics[id]}
		if picklistDim != "" {
			if p.picklist, err = leafCodes(ctx, pool, picklistDim, nil); err != nil {
				return nil, err
			}
			if len(p.picklist) == 0 {
				return nil, fmt.Errorf("metric %s is a pick-list with no options", id)
			}
		} else if p.spec.Value == nil && p.spec.Max < p.spec.Min {
			return nil, fmt.Errorf("metric %s: max is below min", id)
		}
		plans = append(plans, p)
	}

	// Each dimension's leaves, under the chosen members, less what the
	// caller may not write.
	restricted, _, err := restrictedIDs(ctx, pool, req.ModelID, req.RevisionID, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("resolve restricted members: %w", err)
	}
	axes := make([][]string, len(req.DimensionIDs))
	total, all := 1, 1
	for i, dimID := range req.DimensionIDs {
		var under []string
		if codes, ok := req.Members[dimID]; ok {
			if under, err = codesUnder(ctx, pool, dimID, codes); err != nil {
				return nil, err
			}
		}
		leaves, err := leafCodes(ctx, pool, dimID, under)
		if err != nil {
			return nil, err
		}
		writable, err := writableCodes(ctx, pool, dimID, leaves, restricted)
		if err != nil {
			return nil, err
		}
		if len(writable) == 0 {
			return &SampleResult{}, nil
		}
		axes[i] = writable
		total *= len(writable)
		all *= len(leaves)
		if total*len(plans) > req.MaxCells {
			return nil, fmt.Errorf("that is more than %d cells: name members to fill part of the grid", req.MaxCells)
		}
	}

	var filled map[string]bool
	if req.OnlyEmpty {
		if filled, err = filledCells(ctx, pool, req.ModelID, req.RevisionID, ids, req.DimensionIDs); err != nil {
			return nil, err
		}
	}

	rng := rand.New(rand.NewPCG(req.Seed, req.Seed^0x9e3779b97f4a7c15))
	out := &SampleResult{Withheld: (all - total) * len(plans)}
	combo := make([]int, len(axes))
	row := 0
	for {
		codes := make([]string, len(axes))
		for i, a := range axes {
			codes[i] = a[combo[i]]
		}
		key := strings.Join(codes, "\x00")
		for _, p := range plans {
			if filled[p.id+"\x00"+key] {
				continue
			}
			dm := make(map[string]string, len(codes))
			for i, dimID := range req.DimensionIDs {
				dm[dimID] = codes[i]
			}
			row++
			out.Rows = append(out.Rows, StagingRow{MetricID: p.id, DimMembers: dm, Value: p.draw(rng), RowNumber: row})
		}
		// Next combination, the last dimension fastest.
		i := len(combo) - 1
		for ; i >= 0; i-- {
			combo[i]++
			if combo[i] < len(axes[i]) {
				break
			}
			combo[i] = 0
		}
		if i < 0 {
			break
		}
	}
	return out, nil
}

// samplePlan is one metric's draw.
type samplePlan struct {
	id       string
	spec     SampleSpec
	picklist []string // a pick-list's option codes
}

func (p samplePlan) draw(rng *rand.Rand) float64 {
	if len(p.picklist) > 0 {
		return formula.PicklistKey(p.picklist[rng.IntN(len(p.picklist))])
	}
	if p.spec.Value != nil {
		return *p.spec.Value
	}
	v := p.spec.Min + rng.Float64()*(p.spec.Max-p.spec.Min)
	scale := math.Pow(10, float64(max(p.spec.Decimals, 0)))
	return math.Round(v*scale) / scale
}

// leafCodes is dimID's leaf members (no members under them, not
// calculated), in order; within, when given, only those codes.
func leafCodes(ctx context.Context, pool *pgxpool.Pool, dimID string, within []string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT m.code FROM model.dimension_member m
		WHERE m.dimension_id::text = $1 AND NULLIF(btrim(m.formula), '') IS NULL
		  AND NOT EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id)
		  AND ($2::text[] IS NULL OR m.code = ANY($2::text[]))
		ORDER BY m.sort_order, m.code`, dimID, within)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// writableCodes is codes less the members of restricted (member ids).
func writableCodes(ctx context.Context, pool *pgxpool.Pool, dimID string, codes, restricted []string) ([]string, error) {
	if len(restricted) == 0 {
		return codes, nil
	}
	rows, err := pool.Query(ctx, `SELECT code FROM model.dimension_member WHERE dimension_id::text = $1 AND id::text = ANY($2::text[])`, dimID, restricted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	skip := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		skip[c] = true
	}
	out := codes[:0:0]
	for _, c := range codes {
		if !skip[c] {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// filledCells is the cells of metricIDs holding a value, keyed
// metric id + "\x00" + the codes of dimIDs joined by "\x00".
func filledCells(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID string, metricIDs, dimIDs []string) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT metric_id::text, dim_members::text FROM runtime.fact_input
		WHERE model_id = $1::uuid AND revision_id = $2::uuid AND metric_id::text = ANY($3::text[])`,
		modelID, revisionID, metricIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var mid, raw string
		if err := rows.Scan(&mid, &raw); err != nil {
			return nil, err
		}
		var dm map[string]string
		if json.Unmarshal([]byte(raw), &dm) != nil {
			continue
		}
		codes := make([]string, len(dimIDs))
		for i, d := range dimIDs {
			codes[i] = dm[d]
		}
		out[mid+"\x00"+strings.Join(codes, "\x00")] = true
	}
	return out, rows.Err()
}
