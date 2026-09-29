package modeledit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ReorderError is a reorder request that names the wrong members: the
// caller answers it with 400 and its message, as it answers a
// timedim.Error. Anything else ReorderMembers returns is a database failure.
type ReorderError struct{ Msg string }

func (e *ReorderError) Error() string { return e.Msg }

func reorderErr(format string, args ...any) error {
	return &ReorderError{Msg: fmt.Sprintf(format, args...)}
}

// ErrTimeOrder refuses a reorder on a time dimension: its periods are
// ordered by their dates (time_index), which every time function reads.
const ErrTimeOrder = "time periods keep calendar order"

// ReorderResult is what a reorder changed, for the caller's audit record.
type ReorderResult struct {
	// ParentCode is the code of the parent whose children were reordered,
	// "" for the dimension's top-level members.
	ParentCode string
	// Codes are the reordered members' codes in their new order.
	Codes []string
}

// ReorderMembers sets the order of one level of a dimension's hierarchy —
// PUT /api/developer/dimensions/{id}/members/order and the AI Developer's
// reorder_dimension_members.
//
// memberIDs must be exactly the members of the dimension whose parent is
// parentID (nil: the members with no parent), each once. parentID is a
// member of the dimension, or a member outside it that members of the
// dimension hang under. Which side a level's parent sits on follows the
// members, not the declared parent dimension: parent_dimension_id can be
// declared or cleared after the members exist. A member of the declared
// parent dimension with no children here is an empty level.
//
// sort_order is the order every reader shows members in — grids, pickers,
// charts, exports — and they all read it flat (ORDER BY sort_order, code).
// So the whole dimension is renumbered 1..N in depth-first tree order: the
// top-level members in order, each followed by its children in their order,
// recursively. A member whose parent sits outside the dimension counts as
// top-level here; when such a group is the one reordered, its members take
// the positions the group held, so the other groups do not move.
//
// Tree order holds from this renumbering until the next member add or
// re-parent: those append (MAX(sort_order)+1), so the new member lists after
// the last subtree until a reorder renumbers the dimension again
// (docs/OBSERVATIONS.md). Sibling order is kept either way.
//
// The order is a property of the dimension row, and a dimension row belongs
// to one revision: the edit stays inside that revision, and revision
// duplication and model export carry sort_order with the members.
func ReorderMembers(ctx context.Context, db DB, dimID string, parentID *string, memberIDs []string) (ReorderResult, error) {
	var res ReorderResult
	tx, err := db.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	// The dimension row FOR UPDATE serializes reorders of one dimension and
	// holds back a member insert, whose dimension_id foreign-key check waits
	// for this lock. A re-parent, rename or delete of a member touches no
	// dimension row; the member rows are locked below for those.
	var dimType string
	var parentDimID *string
	if err := tx.QueryRow(ctx, `
		SELECT dimension_type, parent_dimension_id::text FROM model.dimension_def WHERE id=$1::uuid FOR UPDATE`,
		dimID).Scan(&dimType, &parentDimID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return res, fmt.Errorf("dimension not found: %w", pgx.ErrNoRows)
		}
		return res, err
	}
	if dimType == "time" {
		return res, &ReorderError{Msg: ErrTimeOrder}
	}

	type member struct {
		id, code, parent string // parent "" = none
		sortOrder        int
	}
	// FOR NO KEY UPDATE: a member re-parented, renamed or deleted while this
	// runs either commits first — and this read, at READ COMMITTED, sees the
	// row as it committed — or waits until the renumbering commits. Without
	// it the tree was read before a re-parent and the renumbering written
	// after it, out of tree order.
	rows, err := tx.Query(ctx, `
		SELECT id::text, code, COALESCE(parent_member_id::text, ''), sort_order
		FROM model.dimension_member WHERE dimension_id=$1::uuid
		FOR NO KEY UPDATE`, dimID)
	if err != nil {
		return res, err
	}
	var all []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.id, &m.code, &m.parent, &m.sortOrder); err != nil {
			rows.Close()
			return res, err
		}
		all = append(all, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	// The current order, sorted here: a locking read's ORDER BY can return
	// rows out of order when it waited on one.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].sortOrder != all[j].sortOrder {
			return all[i].sortOrder < all[j].sortOrder
		}
		return all[i].code < all[j].code
	})
	byID := make(map[string]int, len(all))
	for i, m := range all {
		byID[m.id] = i
	}

	// The parent: a member of the dimension; or a member outside it that
	// members here hang under; or, for an empty level, a member of the
	// declared parent dimension.
	parent := ""
	if parentID != nil {
		parent = strings.ToLower(strings.TrimSpace(*parentID))
		if i, ok := byID[parent]; ok {
			res.ParentCode = all[i].code
		} else {
			hasChildren := false
			for _, m := range all {
				if m.parent == parent {
					hasChildren = true
					break
				}
			}
			var code string
			ok := false
			switch {
			case !uuidShaped(parent):
			case hasChildren:
				ok = tx.QueryRow(ctx, `SELECT code FROM model.dimension_member WHERE id=$1::uuid`, parent).Scan(&code) == nil
			case parentDimID != nil:
				ok = tx.QueryRow(ctx, `SELECT code FROM model.dimension_member WHERE id=$1::uuid AND dimension_id=$2::uuid`,
					parent, *parentDimID).Scan(&code) == nil
			}
			if !ok {
				return res, reorderErr("parent member %q is not a member of this dimension", *parentID)
			}
			res.ParentCode = code
		}
	}

	// memberIDs against the parent's current children.
	var children []string // current order
	isChild := map[string]bool{}
	for _, m := range all {
		if m.parent == parent {
			children = append(children, m.id)
			isChild[m.id] = true
		}
	}
	level := "the top-level members"
	if parent != "" {
		level = fmt.Sprintf("the children of %q", res.ParentCode)
	}
	wanted := make([]string, 0, len(memberIDs))
	seen := map[string]bool{}
	for _, raw := range memberIDs {
		id := strings.ToLower(strings.TrimSpace(raw))
		i, inDim := byID[id]
		switch {
		case !inDim:
			return res, reorderErr("member %q is not a member of this dimension", raw)
		case !isChild[id]:
			return res, reorderErr("member %q is not one of %s", all[i].code, level)
		case seen[id]:
			return res, reorderErr("member %q is listed more than once", all[i].code)
		}
		seen[id] = true
		wanted = append(wanted, id)
	}
	if len(wanted) != len(children) {
		var missing []string
		for _, id := range children {
			if !seen[id] {
				missing = append(missing, all[byID[id]].code)
			}
		}
		return res, reorderErr("member_ids must list every one of %s; missing: %s", level, strings.Join(missing, ", "))
	}
	for _, id := range wanted {
		res.Codes = append(res.Codes, all[byID[id]].code)
	}

	// The tree in its current order, with the one level replaced.
	kids := map[string][]string{} // parent id (in this dimension) → children
	var tops []string             // no parent, or a parent outside the dimension
	for _, m := range all {
		if _, internal := byID[m.parent]; m.parent != "" && internal {
			kids[m.parent] = append(kids[m.parent], m.id)
		} else {
			tops = append(tops, m.id)
		}
	}
	if _, internal := byID[parent]; parent != "" && internal {
		kids[parent] = wanted
	} else {
		// The group's members take the slots the group held among the
		// top-level members.
		next := 0
		for i, id := range tops {
			if isChild[id] {
				tops[i] = wanted[next]
				next++
			}
		}
	}

	order := make([]string, 0, len(all))
	placed := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if placed[id] {
			return
		}
		placed[id] = true
		order = append(order, id)
		for _, c := range kids[id] {
			walk(c)
		}
	}
	for _, id := range tops {
		walk(id)
	}
	// A member only a parent cycle reaches (the edit paths refuse cycles,
	// but the renumbering must not drop a member if one exists).
	for _, m := range all {
		walk(m.id)
	}

	ids := make([]string, 0, len(order))
	ords := make([]int32, 0, len(order))
	for i, id := range order {
		if all[byID[id]].sortOrder != i+1 {
			ids = append(ids, id)
			ords = append(ords, int32(i+1))
		}
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE model.dimension_member m SET sort_order = v.ord
			FROM unnest($2::text[], $3::int[]) AS v(id, ord)
			WHERE m.id = v.id::uuid AND m.dimension_id = $1::uuid`,
			dimID, ids, ords); err != nil {
			return res, err
		}
	}
	return res, tx.Commit(ctx)
}

// uuidShaped reports whether s looks like a UUID, so a malformed id is a
// "not a member" answer rather than a cast error.
func uuidShaped(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}
