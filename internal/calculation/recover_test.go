package calculation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// TestRecalcEntryPointsRecover: a panic inside a recalculation (here a nil
// store, dereferenced on the first load) is the entry point's error, never a
// panic in the caller: recalculations run in background goroutines, where
// one would stop the whole process.
func TestRecalcEntryPointsRecover(t *testing.T) {
	s := NewScheduler(zerolog.Nop(), nil, nil)
	ctx := context.Background()
	for name, run := range map[string]func() error{
		"RecalcAffected":            func() error { return s.RecalcAffected(ctx, "m", "r", []string{"x"}) },
		"RecalcSpecific":            func() error { return s.RecalcSpecific(ctx, "m", "r", []string{"x"}) },
		"RecalcDimensionDependents": func() error { return s.RecalcDimensionDependents(ctx, "d") },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "internal error") {
			t.Errorf("%s: want the recovered panic as an error, got %v", name, err)
		}
	}
}

// TestGuardedTurnsPanicIntoError: one metric's calculation that panics is
// that metric's error (marked failed), and an ordinary error passes through.
func TestGuardedTurnsPanicIntoError(t *testing.T) {
	s := NewScheduler(zerolog.Nop(), nil, nil)
	err := s.guarded("margin", func() error { panic("boom") })
	if err == nil || !strings.Contains(err.Error(), "internal error during margin: boom") {
		t.Errorf("want the panic as margin's error, got %v", err)
	}
	plain := errors.New("plain")
	if err := s.guarded("margin", func() error { return plain }); !errors.Is(err, plain) {
		t.Errorf("an ordinary error changed: %v", err)
	}
	if err := s.guarded("margin", func() error { return nil }); err != nil {
		t.Errorf("success became %v", err)
	}
}
