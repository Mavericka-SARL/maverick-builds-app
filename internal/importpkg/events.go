package importpkg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
)

const subjectFactsCommitted = "facts.committed"

// FactsCommittedEvent is published after a successful import commit.
type FactsCommittedEvent struct {
	ModelID    string   `json:"model_id"`
	RevisionID string   `json:"revision_id"`
	MetricIDs  []string `json:"metric_ids"`
	UserID     string   `json:"user_id"`
}

// Publisher publishes domain events to NATS JetStream.
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

// PublishFactsCommitted fires a facts.committed event for the calculation engine.
func (p *Publisher) PublishFactsCommitted(_ context.Context, evt FactsCommittedEvent) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	_, err = p.js.Publish(subjectFactsCommitted, data)
	return err
}
