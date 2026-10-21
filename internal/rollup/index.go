package rollup

import (
	"sync/atomic"
	"unsafe"
)

// memberIndex is a dimension's members by code and by parent code. Every
// read along a dimension asked childrenOf and findMember, each a scan of all
// its members, several times per cell read: on the education FP&A model a
// recalculation spent most of its evaluation time there (2026-10-20).
//
// It is built on first use and kept on the Dimension; a Dimension whose
// Members were replaced (another slice or another length) gets a new one.
// Members are never changed in place once a dimension is read.
type memberIndex struct {
	data     *Member
	n        int
	byCode   map[string]int      // code → index of its first member
	children map[string][]Member // parent code → children, in member order
}

func (d *Dimension) index() *memberIndex {
	data := unsafe.SliceData(d.Members)
	if p := (*memberIndex)(atomic.LoadPointer(&d.idx)); p != nil && p.data == data && p.n == len(d.Members) {
		return p
	}
	p := &memberIndex{
		data:     data,
		n:        len(d.Members),
		byCode:   make(map[string]int, len(d.Members)),
		children: map[string][]Member{},
	}
	for i, m := range d.Members {
		if _, dup := p.byCode[m.Code]; !dup {
			p.byCode[m.Code] = i
		}
		if m.ParentCode != "" {
			p.children[m.ParentCode] = append(p.children[m.ParentCode], m)
		}
	}
	for code, c := range p.children {
		p.children[code] = c[:len(c):len(c)] // a caller's append copies
	}
	atomic.StorePointer(&d.idx, unsafe.Pointer(p))
	return p
}
