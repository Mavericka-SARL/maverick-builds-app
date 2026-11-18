package rollup

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A read whose request has gone stops (2026-11-18): on the Playground a
// dashboard's charts kept working their leaves out after the person had left
// the tab, and every other request waited for the CPU they held. Each of the
// ways a read walks its leaves — a pinned parent's children, every leaf of
// an unpinned dimension, the leaves combined flat, and the listed facts —
// stops within a few hundred leaves of the cancel, with the context's error.
func TestAReadStopsWhenItsRequestIsGone(t *testing.T) {
	const leaves = 5000
	acc := &Dimension{ID: "acc", Members: []Member{{Code: "ALL"}}}
	var listed []map[string]string
	for i := range leaves {
		code := fmt.Sprintf("A%04d", i)
		acc.Members = append(acc.Members, Member{Code: code, ParentCode: "ALL"})
		listed = append(listed, map[string]string{"acc": code})
	}
	dims := map[string]*Dimension{"acc": acc}

	cases := []struct {
		name string
		read func(ctx context.Context, fetch RawValue) error
	}{
		{"a pinned parent's children", func(ctx context.Context, fetch RawValue) error {
			_, _, err := Resolve(ctx, dims, "m", []string{"acc"}, AggSum, map[string]string{"acc": "ALL"}, fetch)
			return err
		}},
		{"every leaf of an unpinned dimension", func(ctx context.Context, fetch RawValue) error {
			_, _, err := Resolve(ctx, dims, "m", []string{"acc"}, AggSum, map[string]string{}, fetch)
			return err
		}},
		{"the leaves combined flat", func(ctx context.Context, fetch RawValue) error {
			_, _, err := ResolveTimeFlat(ctx, dims, "m", []string{"acc"}, AggSum, "sum", map[string]string{"acc": "ALL"}, fetch)
			return err
		}},
		{"the listed facts", func(ctx context.Context, fetch RawValue) error {
			rec := func(context.Context, string) ([]map[string]string, bool, error) { return listed, true, nil }
			_, _, err := Resolve(WithRecorded(ctx, rec), dims, "m", []string{"acc"}, AggSum, map[string]string{}, fetch)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			fetch := func(context.Context, string, map[string]string) (float64, bool, error) {
				calls++
				if calls == 100 {
					cancel() // the person left the tab
				}
				return 1, true, nil
			}
			err := tc.read(ctx, fetch)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if calls > 100+256 {
				t.Errorf("%d of %d leaves read after the cancel at 100, want it to stop within 256", calls, leaves)
			}

			// The same read, never cancelled, reads every leaf.
			calls = 0
			whole := func(context.Context, string, map[string]string) (float64, bool, error) {
				calls++
				return 1, true, nil
			}
			if err := tc.read(context.Background(), whole); err != nil || calls != leaves {
				t.Errorf("uncancelled: err %v after %d reads, want every one of %d", err, calls, leaves)
			}
		})
	}
}
