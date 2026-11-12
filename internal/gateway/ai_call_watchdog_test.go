package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

// A call that sends nothing for the limit is stopped and reported stalled; a
// call that keeps sending runs past the limit; a parent's cancellation is
// not a stall.
func TestWatchedAICallStopsASilentCall(t *testing.T) {
	limit := 40 * time.Millisecond
	stalled, err := watchedAICall(context.Background(), limit, 0, func(ctx context.Context, _ func()) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !stalled || err == nil {
		t.Fatalf("silent call: stalled=%v err=%v, want stalled with an error", stalled, err)
	}

	stalled, err = watchedAICall(context.Background(), limit, 0, func(ctx context.Context, touch func()) error {
		for i := 0; i < 8; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(limit / 4):
				touch()
			}
		}
		return nil
	})
	if stalled || err != nil {
		t.Fatalf("talking call: stalled=%v err=%v, want it to finish", stalled, err)
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	stalled, err = watchedAICall(parent, limit, 0, func(ctx context.Context, _ func()) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if stalled || !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped turn: stalled=%v err=%v, want a cancellation, not a stall", stalled, err)
	}
}

// A call that keeps sending but runs past the total limit is stopped and
// reported stalled, as the 15-minute calls of 2026-11-11 should have been.
func TestWatchedAICallStopsALongTalkingCall(t *testing.T) {
	idle, total := 40*time.Millisecond, 120*time.Millisecond
	start := time.Now()
	stalled, err := watchedAICall(context.Background(), idle, total, func(ctx context.Context, touch func()) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(idle / 4):
				touch()
			}
		}
	})
	if !stalled || err == nil {
		t.Fatalf("long call: stalled=%v err=%v, want stalled with an error", stalled, err)
	}
	if took := time.Since(start); took > 10*total {
		t.Fatalf("long call ran %s, want it stopped near %s", took, total)
	}
}

func TestTransientAICallError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errors.New(`openai: read tcp 10.0.0.1:443: read: connection reset by peer`), true},
		{fmt.Errorf("openai: %w", io.ErrUnexpectedEOF), true},
		{errors.New("openai: 400 Bad Request: model not found"), false},
		{nil, false},
	} {
		if got := transientAICallError(c.err); got != c.want {
			t.Errorf("transientAICallError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
