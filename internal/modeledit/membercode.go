package modeledit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// FirstRevisionName is the revision a new model is created with. It is not
// made active: activation is the developer's own decision.
const FirstRevisionName = "Revision 1"

// MaxGeneratedCodeLen is the longest member code the engine makes up for a
// member that came without one. A code the developer types may be longer.
const MaxGeneratedCodeLen = 10

// Querier is the one call MemberCode needs: the pool or a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// MemberCodeBase is the code a label suggests: its letters and digits,
// uppercased, words joined by "_", cut to MaxGeneratedCodeLen. A label with
// no latin letter or digit gets "M_" and a hash of the label.
func MemberCodeBase(label string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.TrimSpace(label) {
		switch {
		case r >= 'a' && r <= 'z':
			r -= 'a' - 'A'
			fallthrough
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			if gap && b.Len() > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			gap = false
		default:
			gap = true
		}
	}
	base := b.String()
	if base == "" {
		sum := sha256.Sum256([]byte(label))
		base = "M_" + strings.ToUpper(hex.EncodeToString(sum[:4]))
	}
	return trimCode(base, MaxGeneratedCodeLen)
}

func trimCode(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.TrimRight(s, "_")
}

// MemberCode is the code for a member of dimensionID labelled label that
// arrived without one. A member already carrying that label keeps its code,
// so a re-import updates it in place; otherwise NewMemberCode.
func MemberCode(ctx context.Context, db Querier, dimensionID, label string) (string, error) {
	var code string
	err := db.QueryRow(ctx,
		`SELECT code FROM model.dimension_member WHERE dimension_id=$1::uuid AND label=$2 ORDER BY code LIMIT 1`,
		dimensionID, label).Scan(&code)
	if err == nil {
		return code, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	return NewMemberCode(ctx, db, dimensionID, label)
}

// NewMemberCode is FreeMemberCode against the codes dimensionID's members
// already have: the code for a member being added with no code of its own.
func NewMemberCode(ctx context.Context, db Querier, dimensionID, label string) (string, error) {
	return FreeMemberCode(label, func(c string) (bool, error) {
		var taken bool
		err := db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2)`,
			dimensionID, c).Scan(&taken)
		return taken, err
	})
}

// FreeMemberCode is MemberCodeBase(label), or the first of BASE_2, BASE_3, …
// (shortened to stay within MaxGeneratedCodeLen) that taken refuses.
func FreeMemberCode(label string, taken func(code string) (bool, error)) (string, error) {
	base := MemberCodeBase(label)
	candidate := base
	for i := 2; i < 1000; i++ {
		t, err := taken(candidate)
		if err != nil {
			return "", err
		}
		if !t {
			return candidate, nil
		}
		suffix := fmt.Sprintf("_%d", i)
		candidate = trimCode(base, MaxGeneratedCodeLen-len(suffix)) + suffix
	}
	return "", fmt.Errorf("could not derive a unique code for label %q", label)
}
