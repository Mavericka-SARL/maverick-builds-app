package aiassistant

import (
	"context"
	"math"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// SessionUsage is what a session's calls to the AI provider were billed for,
// as the provider reported it (migration 160).
type SessionUsage struct {
	Calls            int   `json:"llm_calls"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

// ModelPrice is what a model costs per million tokens, in US dollars.
type ModelPrice struct {
	Input  float64 `json:"input"`
	Cached float64 `json:"cached"`
	Output float64 `json:"output"`
}

// EstimatedCost is the usage priced at p (cache writes as input), rounded
// to the cent.
func (u SessionUsage) EstimatedCost(p ModelPrice) float64 {
	c := (float64(u.InputTokens+u.CacheWriteTokens)*p.Input + float64(u.CacheReadTokens)*p.Cached + float64(u.OutputTokens)*p.Output) / 1e6
	return math.Round(c*100) / 100
}

// Total is every token billed: input, cached and written to the cache,
// and output.
func (u SessionUsage) Total() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// AddUsage adds one call's usage to sessionID's totals.
func (s *ChatStore) AddUsage(ctx context.Context, sessionID string, u providers.Usage) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE ai_assistant.session SET llm_calls = llm_calls + 1,
		       input_tokens = input_tokens + $2, output_tokens = output_tokens + $3,
		       cache_read_tokens = cache_read_tokens + $4, cache_write_tokens = cache_write_tokens + $5
		WHERE id = $1::uuid`, sessionID, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	return err
}

// Usage is sessionID's totals so far.
func (s *ChatStore) Usage(ctx context.Context, sessionID string) (SessionUsage, error) {
	var u SessionUsage
	err := s.pool.QueryRow(ctx, `
		SELECT llm_calls, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
		FROM ai_assistant.session WHERE id = $1::uuid`, sessionID).
		Scan(&u.Calls, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &u.CacheWriteTokens)
	return u, err
}
