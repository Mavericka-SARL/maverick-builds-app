package writeguard

import (
	"context"
	"slices"
	"sync"
)

// Memo keeps what the write guard read for one batch of writes: each
// member's ancestor chain, each access rule a user has on a member or a
// metric, the member each code names, whether each revision is
// system-managed. The workflow and revision locks are read afresh for each
// check. A file of 900 GL rows checks 180 cells over 29 members; each check
// used to walk every member's chain and rules again, about 35 statements.
// What a batch has read holds while it runs: a rule or a member changed
// meanwhile applies from the next batch, as when it changes just after one.
type Memo struct {
	mu      sync.Mutex
	chains  map[string][]AncestorRef
	access  map[[3]string]string
	managed map[string]bool
	members map[[4]string]resolvedMember
	// loaded are the users whose member and metric rules in the batch's
	// revision were all read at once (PreloadRules): a member or metric with
	// no rule among them has none, without asking the database.
	loaded map[string]bool
}

// resolvedMember is what ResolveWriteMembers read for one code.
type resolvedMember struct {
	unknownDim              bool
	dimName, memberID       string
	hasChildren, calculated bool
}

// NewMemo is an empty Memo.
func NewMemo() *Memo {
	return &Memo{
		chains: map[string][]AncestorRef{}, access: map[[3]string]string{},
		managed: map[string]bool{}, members: map[[4]string]resolvedMember{},
		loaded: map[string]bool{},
	}
}

// PreloadRules reads every member and metric rule userID has in revisionID —
// their own and their roles', resolved by lineage as every read path does
// (RulesForRevision) — so the batch's access checks answer from memory. A
// project manager with 84 read rules wrote 500 progress cells in more than 20
// minutes: each member of each cell asked the rule view on its own, 35–60 ms a
// question (2026-11-27). Only for writes into that one revision: a member of
// another revision would read as having no rule.
func (m *Memo) PreloadRules(ctx context.Context, q Querier, userID, revisionID string) error {
	if userID == "" || revisionID == "" {
		return nil
	}
	rules, err := RulesForRevision(ctx, q, userID, revisionID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rules {
		if r.Type != "dimension_member" && r.Type != "metric" {
			continue
		}
		key := [3]string{userID, r.Type, r.RefID}
		if cur, ok := m.access[key]; !ok || Stricter(r.Access, cur) {
			m.access[key] = r.Access
		}
	}
	m.loaded[userID] = true
	return nil
}

func (m *Memo) rulesLoaded(userID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loaded[userID]
}

type memoKey struct{}

// WithMemo is ctx with the write guard's checks under it reading through
// memo.
func WithMemo(ctx context.Context, memo *Memo) context.Context {
	return context.WithValue(ctx, memoKey{}, memo)
}

func memoOf(ctx context.Context) *Memo {
	m, _ := ctx.Value(memoKey{}).(*Memo)
	return m
}

func (m *Memo) chain(memberID string) ([]AncestorRef, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.chains[memberID]
	return slices.Clone(c), ok
}

func (m *Memo) setChain(memberID string, chain []AncestorRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chains[memberID] = slices.Clone(chain)
}

func (m *Memo) rule(key [3]string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.access[key]
	return a, ok
}

func (m *Memo) setRule(key [3]string, access string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.access[key] = access
}

func (m *Memo) systemManaged(revisionID string) (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.managed[revisionID]
	return v, ok
}

func (m *Memo) setSystemManaged(revisionID string, v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.managed[revisionID] = v
}

func (m *Memo) member(key [4]string) (resolvedMember, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.members[key]
	return r, ok
}

func (m *Memo) setMember(key [4]string, r resolvedMember) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.members[key] = r
}
