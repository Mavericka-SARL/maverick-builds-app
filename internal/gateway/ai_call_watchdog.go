package gateway

import (
	"context"
	"errors"
	"os"
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

// watchedAICall runs call under a context that is cancelled when call has
// not called touch for limit. stalled reports that this, not the parent or
// the provider, ended it.
func watchedAICall(parent context.Context, limit time.Duration, call func(ctx context.Context, touch func()) error) (stalled bool, err error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	timer := time.AfterFunc(limit, func() { cancel(errAICallStalled) })
	defer timer.Stop()
	err = call(ctx, func() { timer.Reset(limit) })
	stalled = err != nil && parent.Err() == nil && errors.Is(context.Cause(ctx), errAICallStalled)
	return stalled, err
}
