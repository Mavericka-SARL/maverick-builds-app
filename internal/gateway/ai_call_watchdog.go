package gateway

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

// errAICallStalled ends a provider call that has sent nothing for
// aiCallIdleLimit.
var errAICallStalled = errors.New("the AI provider sent nothing for too long")

// aiCallIdleLimit bounds how long one provider call may go without sending
// anything (a text token, a tool argument, its end). Only the turn's limit
// (aiTurnTimeout, 30 min) bounded it, and a call whose stream opened and then
// fell silent held the turn for 12 to 19 minutes with nothing on screen
// (gpt-5-mini, 2026-10-31). AI_CALL_IDLE_TIMEOUT overrides it (a Go
// duration, e.g. "8m").
func aiCallIdleLimit() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("AI_CALL_IDLE_TIMEOUT")); err == nil && d > 0 {
		return d
	}
	return 5 * time.Minute
}

// errAICallTooLong ends a provider call that has run for aiCallMaxDuration
// however much it sent: a stream that keeps trickling resets the idle limit,
// and one call held a turn for 15 min 41 s and another for 15 min 52 s
// (gpt-5-mini, 2026-11-11) with nothing to show for it.
var errAICallTooLong = errors.New("the AI provider call ran too long")

// aiCallMaxDuration bounds one provider call in all, whatever it sends.
// AI_CALL_MAX_DURATION overrides it (a Go duration, e.g. "10m").
func aiCallMaxDuration() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("AI_CALL_MAX_DURATION")); err == nil && d > 0 {
		return d
	}
	return 6 * time.Minute
}

// watchedAICall runs call under a context that is cancelled when call has
// not called touch for limit, or has run for maxDur in all (0: no total
// limit). stalled reports that this, not the parent or the provider, ended
// it.
func watchedAICall(parent context.Context, limit, maxDur time.Duration, call func(ctx context.Context, touch func()) error) (stalled bool, err error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	timer := time.AfterFunc(limit, func() { cancel(errAICallStalled) })
	defer timer.Stop()
	if maxDur > 0 {
		total := time.AfterFunc(maxDur, func() { cancel(errAICallTooLong) })
		defer total.Stop()
	}
	err = call(ctx, func() { timer.Reset(limit) })
	cause := context.Cause(ctx)
	stalled = err != nil && parent.Err() == nil && (errors.Is(cause, errAICallStalled) || errors.Is(cause, errAICallTooLong))
	return stalled, err
}

// transientAICallError reports a provider call that ended on a dropped
// connection rather than an answer ("connection reset by peer" after a
// 15-minute call, 2026-11-11): asked once more, it usually answers.
func transientAICallError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	low := strings.ToLower(err.Error())
	for _, s := range []string{"connection reset", "unexpected eof", "broken pipe", "stream error", "http2: server sent goaway", "internal_error"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}
