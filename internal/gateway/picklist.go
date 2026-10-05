package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// picklistOption is one member a pick-list cell can hold: the key the cell
// stores (formula.PicklistKey of the code), the code formulas read, and the
// label people see.
type picklistOption struct {
	Key   float64 `json:"key"`
	Code  string  `json:"code"`
	Label string  `json:"label"`
	// Parent: the member has members under it. Listed so a cell already
	// holding one shows its label; offered and accepted only where the
	// metric allows parents (picklist_allow_parents).
	Parent bool `json:"parent,omitempty"`
}

// picklistMembers returns the members of dimID a pick-list cell can hold, in
// the dimension's order. Calculated members hold no values and are left out.
func (h *handler) picklistMembers(ctx context.Context, dimID string) ([]picklistOption, error) {
	rows, err := h.db.Query(ctx, `
		SELECT m.code, COALESCE(NULLIF(btrim(m.label),''), m.code),
		       EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id)
		FROM model.dimension_member m
		WHERE m.dimension_id = $1::uuid AND NULLIF(btrim(m.formula),'') IS NULL
		ORDER BY m.time_index NULLS LAST, m.sort_order, m.code`, dimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []picklistOption
	for rows.Next() {
		var o picklistOption
		if err := rows.Scan(&o.Code, &o.Label, &o.Parent); err != nil {
			return nil, err
		}
		o.Key = formula.PicklistKey(o.Code)
		out = append(out, o)
	}
	return out, rows.Err()
}

// attachPicklistOptions puts each pick-list metric's members on its row, so
// the grid shows a cell's member and offers the others.
func (h *handler) attachPicklistOptions(ctx context.Context, rows []metricRow) {
	cache := map[string][]picklistOption{}
	for i := range rows {
		dimID := rows[i].PicklistDimensionID
		if dimID == "" {
			continue
		}
		opts, ok := cache[dimID]
		if !ok {
			var err error
			if opts, err = h.picklistMembers(ctx, dimID); err != nil {
				h.log.Warn().Err(err).Str("dimension_id", dimID).Msg("load pick-list members")
			}
			cache[dimID] = opts
		}
		rows[i].PicklistOptions = opts
		_ = h.db.QueryRow(ctx, `SELECT picklist_allow_parents FROM model.metric_def WHERE id = $1::uuid`, rows[i].ID).Scan(&rows[i].PicklistAllowParents)
	}
}

// metricPicklist returns the dimension a pick-list metric's cells hold
// members of, and its name; "" for any other metric.
func (h *handler) metricPicklist(ctx context.Context, metricID string) (dimID, dimName string, allowParents bool, err error) {
	err = h.db.QueryRow(ctx, `
		SELECT COALESCE(m.picklist_dimension_id::text,''), COALESCE(d.name,''), m.picklist_allow_parents
		FROM model.metric_def m LEFT JOIN model.dimension_def d ON d.id = m.picklist_dimension_id
		WHERE m.id = $1::uuid`, metricID).Scan(&dimID, &dimName, &allowParents)
	return dimID, dimName, allowParents, err
}

// picklistValue checks a value written to a pick-list metric and returns
// the key to store: member names one by code or label (matched exactly, then
// ignoring case), a numeric value must be a member's key, and 0 clears the
// cell. For any other metric member must be empty and value is returned as
// it is.
func (h *handler) picklistValue(ctx context.Context, metricID string, value float64, member *string) (float64, error) {
	dimID, dimName, allowParents, err := h.metricPicklist(ctx, metricID)
	if err != nil {
		return 0, err
	}
	if dimID == "" {
		if member != nil {
			return 0, fmt.Errorf("member is for a pick-list metric; this metric holds numbers — send value")
		}
		return value, nil
	}
	opts, err := h.picklistMembers(ctx, dimID)
	if err != nil {
		return 0, err
	}
	parentOf := func(key float64) error {
		for _, o := range opts {
			if o.Key == key && o.Parent && !allowParents {
				return fmt.Errorf("%s (%s) has members under it — a cell of this pick-list holds one of them, not a total (or allow parents on the metric)", o.Label, o.Code)
			}
		}
		return nil
	}
	if member != nil {
		if key, ok := matchPicklistOption(opts, *member); ok {
			if err := parentOf(key); err != nil {
				return 0, err
			}
			return key, nil
		}
		if strings.TrimSpace(*member) == "" {
			return 0, nil
		}
		return 0, fmt.Errorf("%s has no member %q — a pick-list cell holds a member of %s (%s)", dimName, *member, dimName, optionList(opts))
	}
	if value == 0 {
		return 0, nil
	}
	for _, o := range opts {
		if o.Key == value {
			if err := parentOf(value); err != nil {
				return 0, err
			}
			return value, nil
		}
	}
	return 0, fmt.Errorf("this metric is a pick-list of %s and %v is no member's key — send member with a code or label of %s (%s)", dimName, value, dimName, optionList(opts))
}

// matchPicklistOption finds the option named by text: a code exactly, then a
// code or a label ignoring case.
func matchPicklistOption(opts []picklistOption, text string) (float64, bool) {
	t := strings.TrimSpace(text)
	for _, o := range opts {
		if o.Code == t {
			return o.Key, true
		}
	}
	for _, o := range opts {
		if strings.EqualFold(o.Code, t) || strings.EqualFold(o.Label, t) {
			return o.Key, true
		}
	}
	return 0, false
}

// optionList names up to ten options for a message.
func optionList(opts []picklistOption) string {
	var names []string
	for i, o := range opts {
		if i == 10 {
			names = append(names, "…")
			break
		}
		names = append(names, o.Code)
	}
	if len(names) == 0 {
		return "it has no members yet"
	}
	return strings.Join(names, ", ")
}

// nameKindErr answers a name refused by CheckNameFreeOfOtherKind with 409,
// any other error with 500.
func nameKindErr(w http.ResponseWriter, err error) {
	if metricformula.IsValidationError(err) {
		jsonErr(w, err, http.StatusConflict)
		return
	}
	jsonErr(w, err, http.StatusInternalServerError)
}
