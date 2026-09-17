package query

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
)

const subjectFactsCommitted = "facts.committed"

// FactsCommittedEvent is published after a successful writeback commit.
type FactsCommittedEvent struct {
	ModelID    string   `json:"model_id"`
	RevisionID string   `json:"revision_id"`
	MetricIDs  []string `json:"metric_ids"`
	UserID     string   `json:"user_id"`
}

// Publisher wraps a NATS JetStream context for publishing domain events.
type Publisher struct {
	js nats.JetStreamContext
}

func NewPublisher(nc *nats.Conn) (*Publisher, error) {
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream context: %w", err)
	}
	return &Publisher{js: js}, nil
}

// PublishFactsCommitted publishes a facts.committed event to NATS JetStream.
func (p *Publisher) PublishFactsCommitted(ctx context.Context, evt FactsCommittedEvent) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	_, err = p.js.Publish(subjectFactsCommitted, data)
	return err
}
