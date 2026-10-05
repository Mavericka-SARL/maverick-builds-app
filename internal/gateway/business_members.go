package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/modeledit"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// Business-maintained dimensions (migration 111). A developer marks a
// dimension business_maintained; business users then add, rename and remove
// its members from the grid — a planner naming a new strategic activity.
// Only the members' labels and declared properties are theirs: codes are
// chosen at creation (generated when not given), and the dimension itself,
// its properties, parents and periods stay the developer's.
//
//	POST   /api/dimensions/{dimId}/members              {label, code?, parent_member_id?, properties?}
//	PATCH  /api/dimensions/{dimId}/members/{memberId}   {label?, properties?}
//	DELETE /api/dimensions/{dimId}/members/{memberId}
//
// Each is held to the business user's own view: the dimension's revision must
// be the open one, and the write guard (a system-managed revision, hidden and
// read-only members, cascading through parents, a workflow lock) applies to
// the parent a member is added under and to the member renamed or removed.

// errNotBusinessMaintained refuses a member write on a dimension the
// developer has not opened to business users.
var errNotBusinessMaintained = errors.New("this dimension's members are maintained by the model's developer")

// markBusinessMaintained sets BusinessMaintained on the dimensions a
// developer opened to business users.
func (h *handler) markBusinessMaintained(ctx context.Context, dims []gridDimension) {
	if len(dims) == 0 {
		return
	}
	ids := make([]string, len(dims))
	for i, d := range dims {
		ids[i] = d.ID
	}
	rows, err := h.db.Query(ctx, `SELECT id::text FROM model.dimension_def WHERE id::text = ANY($1::text[]) AND business_maintained`, ids)
	if err != nil {
		return
	}
	defer rows.Close()
	open := map[string]bool{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			open[id] = true
		}
	}
	for i := range dims {
		dims[i].BusinessMaintained = open[dims[i].ID]
	}
}

// businessMember is a member write's target, once checked.
type businessMember struct {
	act        *actor
	dimID      string
	modelID    string
	revisionID string
}

// businessMemberAction serves the three routes above.
func (h *handler) businessMemberAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/dimensions/")
	parts := strings.Split(tail, "/")
	if len(parts) < 2 || parts[1] != "members" || len(parts) > 3 {
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
		return
	}
	dimID := parts[0]
	memberID := ""
	if len(parts) == 3 {
		memberID = parts[2]
	}
	bm, ok := h.checkBusinessMember(w, r, dimID, memberID)
	if !ok {
		return
	}
	switch {
	case r.Method == http.MethodPost && memberID == "":
		h.businessAddMember(w, r, bm)
	case r.Method == http.MethodPatch && memberID != "":
		h.businessUpdateMember(w, r, bm, memberID)
	case r.Method == http.MethodDelete && memberID != "":
		h.businessDeleteMember(w, r, bm, memberID)
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// checkBusinessMember holds a member write to the caller's view: the
// dimension is business-maintained and in a model they open, on the open
// revision, and memberID (when given) is one of its members.
func (h *handler) checkBusinessMember(w http.ResponseWriter, r *http.Request, dimID, memberID string) (businessMember, bool) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return businessMember{}, false
	}
	bm := businessMember{act: act, dimID: dimID}
	var maintained bool
	if err := h.db.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text,''), business_maintained
		FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&bm.modelID, &bm.revisionID, &maintained); err != nil {
		jsonErr(w, fmt.Errorf("dimension not found"), http.StatusNotFound)
		return businessMember{}, false
	}
	if canAccess, caErr := h.actorCanAccessModel(ctx, act, bm.modelID); caErr != nil || !canAccess {
		jsonErr(w, fmt.Errorf("dimension not found"), http.StatusNotFound)
		return businessMember{}, false
	}
	if !h.revisionOpen(ctx, act, bm.modelID, bm.revisionID) {
		jsonErr(w, fmt.Errorf("dimension not found"), http.StatusNotFound)
		return businessMember{}, false
	}
	if !maintained {
		jsonErr(w, errNotBusinessMaintained, http.StatusForbidden)
		return businessMember{}, false
	}
	if memberID != "" {
		var inDim bool
		if err := h.db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM model.dimension_member WHERE id=$1::uuid AND dimension_id=$2::uuid)`,
			memberID, dimID).Scan(&inDim); err != nil || !inDim {
			jsonErr(w, fmt.Errorf("member not found"), http.StatusNotFound)
			return businessMember{}, false
		}
	}
	return bm, true
}

// guardMemberWrite runs the write guard over the members a write touches.
func (h *handler) guardMemberWrite(w http.ResponseWriter, r *http.Request, bm businessMember, memberIDs []string) bool {
	ctx := r.Context()
	reason, err := writeguard.CheckWrite(ctx, h.db.For(ctx), bm.modelID, bm.revisionID, bm.act.UserID, memberIDs)
	if err != nil {
		jsonErr(w, fmt.Errorf("write guard: %w", err), http.StatusInternalServerError)
		return false
	}
	if reason != "" {
		jsonErr(w, fmt.Errorf("%s", reason), http.StatusForbidden)
		return false
	}
	return true
}

func (h *handler) businessAddMember(w http.ResponseWriter, r *http.Request, bm businessMember) {
	ctx := r.Context()
	var body struct {
		Code           string            `json:"code"`
		Label          string            `json:"label"`
		ParentMemberID *string           `json:"parent_member_id"`
		Properties     map[string]string `json:"properties"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	body.Label = strings.TrimSpace(body.Label)
	body.Code = strings.TrimSpace(body.Code)
	if body.Label == "" {
		jsonErr(w, fmt.Errorf("label is required: the name the new member shows"), http.StatusBadRequest)
		return
	}
	if body.ParentMemberID != nil && *body.ParentMemberID == "" {
		body.ParentMemberID = nil
	}
	if err := h.validateMemberParent(ctx, bm.dimID, body.ParentMemberID); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	guarded := []string{}
	if body.ParentMemberID != nil {
		guarded = append(guarded, *body.ParentMemberID)
	}
	if !h.guardMemberWrite(w, r, bm, guarded) {
		return
	}
	props, err := metricformula.CheckMemberProperties(ctx, h.db.For(ctx), bm.dimID, body.Properties, nil)
	if err != nil {
		if metricformula.IsValidationError(err) {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if cid, _ := h.customerOfDimension(ctx, bm.dimID); cid != "" && h.plans != nil {
		if err := h.plans.CheckMembers(ctx, h.db.For(ctx), cid, bm.dimID, 1); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}
	if body.Code == "" {
		if body.Code, err = h.nextMemberCode(ctx, bm.dimID, body.Label); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	propsJSON := []byte("{}")
	if len(props) > 0 {
		propsJSON, _ = json.Marshal(props)
	}
	var newID string
	if err := h.db.QueryRow(ctx, `
		INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order, properties)
		VALUES ($1::uuid, $2, $3, $4::uuid,
		        (SELECT COALESCE(MAX(sort_order),0)+1 FROM model.dimension_member WHERE dimension_id=$1::uuid), $5::jsonb)
		RETURNING id::text`, bm.dimID, body.Code, body.Label, body.ParentMemberID, string(propsJSON)).Scan(&newID); err != nil {
		if metricformula.IsMemberCodeTaken(err) {
			jsonErr(w, metricformula.MemberCodeTaken(err, body.Code), http.StatusConflict)
			return
		}
		if isCheckViolation(err) {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if body.ParentMemberID != nil {
		// A leaf that gets its first child hands its values down to it, as
		// the developer's add does.
		var childCount int
		_ = h.db.QueryRow(ctx,
			`SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND parent_member_id=$2::uuid`,
			bm.dimID, *body.ParentMemberID).Scan(&childCount)
		if childCount == 1 {
			if affected, splitErr := h.splitParentFactData(ctx, bm.dimID, *body.ParentMemberID, body.Code, bm.modelID); splitErr == nil {
				go h.recalcAfterDimChange(context.WithoutCancel(ctx), bm.modelID, affected)
			}
		}
	}
	go h.recalcDimensionDependents(context.WithoutCancel(ctx), bm.dimID) //nolint:contextcheck
	h.auditDimensionUpdated(ctx, r, bm.dimID, "member_added", map[string]string{"member_id": newID, "code": body.Code, "by": "business_user"})
	jsonOK(w, map[string]string{"id": newID, "code": body.Code})
}

func (h *handler) businessUpdateMember(w http.ResponseWriter, r *http.Request, bm businessMember, memberID string) {
	ctx := r.Context()
	var body struct {
		Label      *string           `json:"label"`
		Properties map[string]string `json:"properties"`
		// Code and parent are the developer's; sent, they are refused
		// rather than dropped.
		Code           *string `json:"code"`
		ParentMemberID *string `json:"parent_member_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	if body.Code != nil || body.ParentMemberID != nil {
		jsonErr(w, fmt.Errorf("a member's code and parent are the developer's to change; send label and properties"), http.StatusBadRequest)
		return
	}
	if body.Label != nil && strings.TrimSpace(*body.Label) == "" {
		jsonErr(w, fmt.Errorf("label cannot be empty"), http.StatusBadRequest)
		return
	}
	if calc, _ := h.memberIsCalculated(ctx, memberID); calc {
		jsonErr(w, fmt.Errorf("a calculated member is the developer's"), http.StatusForbidden)
		return
	}
	if !h.guardMemberWrite(w, r, bm, []string{memberID}) {
		return
	}
	if body.Properties != nil {
		var stored map[string]string
		var storedJSON []byte
		if err := h.db.QueryRow(ctx, `SELECT COALESCE(properties,'{}'::jsonb) FROM model.dimension_member WHERE id=$1::uuid`, memberID).Scan(&storedJSON); err == nil {
			_ = json.Unmarshal(storedJSON, &stored)
		}
		props, err := metricformula.CheckMemberProperties(ctx, h.db.For(ctx), bm.dimID, body.Properties, stored)
		if err != nil {
			if metricformula.IsValidationError(err) {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		pj, _ := json.Marshal(props)
		if _, err := h.db.Exec(ctx, `UPDATE model.dimension_member SET properties = properties || $2::jsonb WHERE id=$1::uuid`, memberID, string(pj)); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	if body.Label != nil {
		if _, err := h.db.Exec(ctx, `UPDATE model.dimension_member SET label=$2 WHERE id=$1::uuid`, memberID, strings.TrimSpace(*body.Label)); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	if body.Properties != nil {
		// Formulas read properties (dim.property, criteria).
		go h.recalcDimensionDependents(context.WithoutCancel(ctx), bm.dimID) //nolint:contextcheck
	}
	h.auditDimensionUpdated(ctx, r, bm.dimID, "member_updated", map[string]string{"member_id": memberID, "by": "business_user"})
	jsonOK(w, map[string]string{"status": "ok"})
}

func (h *handler) businessDeleteMember(w http.ResponseWriter, r *http.Request, bm businessMember, memberID string) {
	ctx := r.Context()
	var children int
	_ = h.db.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_member WHERE parent_member_id=$1::uuid`, memberID).Scan(&children)
	if children > 0 {
		jsonErr(w, fmt.Errorf("this member has members under it; remove those first"), http.StatusConflict)
		return
	}
	if calc, _ := h.memberIsCalculated(ctx, memberID); calc {
		jsonErr(w, fmt.Errorf("a calculated member is the developer's"), http.StatusForbidden)
		return
	}
	if !h.guardMemberWrite(w, r, bm, []string{memberID}) {
		return
	}
	if err := metricformula.CheckMemberNotInUse(ctx, h.db.For(ctx), bm.dimID, memberID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		if metricformula.IsValidationError(err) {
			jsonErr(w, err, http.StatusConflict)
			return
		}
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if err := h.deleteMemberReindexed(ctx, bm.dimID, memberID); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, modeledit.ErrMemberInPicklist) {
			status = http.StatusConflict
		}
		jsonErr(w, err, status)
		return
	}
	// The member's revision only (its dimension belongs to that one), marked
	// pending until the pass lands: the grid shows the removed row's
	// contribution as about to change instead of as current.
	h.recalcInBackground(ctx, bm.revisionID, "recalculation after a member was removed", func(bg context.Context) {
		if err := h.recalcRevisionFromInputs(bg, bm.modelID, bm.revisionID); err != nil {
			h.log.Warn().Err(err).Str("revision", bm.revisionID).Msg("recalc after a member was removed")
		}
		h.recalcDimensionDependents(bg, bm.dimID)
	})
	h.auditDimensionUpdated(ctx, r, bm.dimID, "member_deleted", map[string]string{"member_id": memberID, "by": "business_user"})
	jsonOK(w, map[string]string{"status": "deleted"})
}

func (h *handler) memberIsCalculated(ctx context.Context, memberID string) (bool, error) {
	var calc bool
	err := h.db.QueryRow(ctx, `SELECT COALESCE(btrim(formula),'') <> '' FROM model.dimension_member WHERE id=$1::uuid`, memberID).Scan(&calc)
	return calc, err
}

// nextMemberCode is the code a business user's new member gets when they
// name none: the next of a numbered sequence the dimension's codes follow
// (INIT-020 → INIT-021), else the label in capitals ("Retail expansion" →
// RETAIL_EXPANSION), made unique with a suffix.
func (h *handler) nextMemberCode(ctx context.Context, dimID, label string) (string, error) {
	rows, err := h.db.Query(ctx, `SELECT code FROM model.dimension_member WHERE dimension_id=$1::uuid`, dimID)
	if err != nil {
		return "", err
	}
	var codes []string
	for rows.Next() {
		var c string
		if rows.Scan(&c) == nil {
			codes = append(codes, c)
		}
	}
	rows.Close()
	return nextCode(codes, label), rows.Err()
}

var numberedCode = regexp.MustCompile(`^(.*?)(\d+)$`)

// nextCode is nextMemberCode over the dimension's codes.
func nextCode(codes []string, label string) string {
	taken := make(map[string]bool, len(codes))
	for _, c := range codes {
		taken[strings.ToLower(c)] = true
	}
	// A sequence: the prefix and digit width most codes share.
	type seq struct {
		prefix        string
		width, max, n int
	}
	seqs := map[string]*seq{}
	var best *seq
	for _, c := range codes {
		m := numberedCode.FindStringSubmatch(c)
		if m == nil || m[1] == "" {
			continue
		}
		key := m[1] + "\x00" + strconv.Itoa(len(m[2]))
		s := seqs[key]
		if s == nil {
			s = &seq{prefix: m[1], width: len(m[2])}
			seqs[key] = s
		}
		s.n++
		if v, err := strconv.Atoi(m[2]); err == nil && v > s.max {
			s.max = v
		}
		if best == nil || s.n > best.n {
			best = s
		}
	}
	if best != nil && best.n*2 > len(codes) {
		for v := best.max + 1; ; v++ {
			c := fmt.Sprintf("%s%0*d", best.prefix, best.width, v)
			if !taken[strings.ToLower(c)] {
				return c
			}
		}
	}
	var b strings.Builder
	under := false
	for _, r := range strings.ToUpper(label) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			under = false
		} else if !under && b.Len() > 0 {
			b.WriteByte('_')
			under = true
		}
	}
	base := strings.TrimRight(b.String(), "_")
	if base == "" {
		base = "MEMBER"
	}
	if rs := []rune(base); len(rs) > 40 {
		base = strings.TrimRight(string(rs[:40]), "_")
	}
	c := base
	for i := 2; taken[strings.ToLower(c)]; i++ {
		c = base + "_" + strconv.Itoa(i)
	}
	return c
}
