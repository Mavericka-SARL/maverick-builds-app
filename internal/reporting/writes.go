package reporting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Writing cells. The one change the connector makes: grid cells, written as
// the person through the engine's batch route (POST /api/cells/batch), which
// checks every cell as the console checks a typed one — input metrics only,
// the person's access rules, workflow locks, the plan's limits — and writes
// all of them or none. Like reads, nothing here decides access.

// Writer makes a write as subject through via, the chat host's client. The
// gateway implements it beside Reader (internal/gateway/mcp.go); it is the
// only way a connector request reaches a write route.
type Writer interface {
	Write(ctx context.Context, subject, via string, req Request) (Response, error)
}

// CodeRefused is a write refused as a whole, nothing written: one or more
// of its cells, or the write itself (writes turned off for the tenant).
const CodeRefused = "refused"

// MaxCellWrites caps one write_cells call, as the batch route does.
const MaxCellWrites = 500

// CellWrite is one cell to write: an input metric at one member of each of
// its dimensions, and exactly one of a number, a text (a text metric's, or a
// date as yyyy-mm-dd), a pick-list member, or a clear.
type CellWrite struct {
	MetricID string `json:"metric_id"`
	// Members fix each of the metric's dimensions to one leaf member
	// (dimension id → member code).
	Members map[string]string `json:"members"`
	Value   *float64          `json:"value,omitempty"`
	Text    *string           `json:"text,omitempty"`
	Member  *string           `json:"member,omitempty"`
	Clear   bool              `json:"clear,omitempty"`
}

// WriteResult is what write_cells did, or would do.
type WriteResult struct {
	Context Pinned `json:"context"`
	// Status is "written", or "valid" for a dry run that wrote nothing.
	Status  string `json:"status"`
	Cells   int    `json:"cells"`
	Cleared int    `json:"cleared"`
	Note    string `json:"note"`
}

// Via binds the session to the chat host's client its writes are recorded
// as made through.
func (s *Session) Via(client string) *Session {
	c := *s
	c.client = client
	return &c
}

// WriteCells writes cells to p's revision as the person, all or nothing; a
// dry run checks every cell and writes none.
func (s *Session) WriteCells(ctx context.Context, p Pinned, cells []CellWrite, dryRun bool) (*WriteResult, error) {
	w := s.s.writer
	if w == nil {
		return nil, &Error{Code: CodeUnsupported, Message: "this connection cannot change data"}
	}
	if s.subject == "" {
		return nil, &Error{Code: CodeUnauthorized, Message: "this connection is not signed in"}
	}
	if len(cells) == 0 || len(cells) > MaxCellWrites {
		return nil, invalid("write 1 to %d cells per call (this call has %d)", MaxCellWrites, len(cells))
	}
	type batchCell struct {
		MetricID string            `json:"metric_id"`
		DimCodes map[string]string `json:"dim_codes"`
		Value    float64           `json:"value"`
		Member   *string           `json:"member,omitempty"`
		Text     *string           `json:"text,omitempty"`
		Clear    bool              `json:"clear,omitempty"`
	}
	body := struct {
		ModelID    string      `json:"model_id"`
		RevisionID string      `json:"revision_id"`
		Cells      []batchCell `json:"cells"`
		DryRun     bool        `json:"dry_run,omitempty"`
	}{ModelID: p.ModelID, RevisionID: p.RevisionID, DryRun: dryRun}
	for i, c := range cells {
		if c.MetricID == "" {
			return nil, invalid("cell %d: metric_id is required", i)
		}
		given := 0
		for _, set := range []bool{c.Value != nil, c.Text != nil, c.Member != nil, c.Clear} {
			if set {
				given++
			}
		}
		if given != 1 {
			return nil, invalid("cell %d: give exactly one of value, text, member or clear", i)
		}
		bc := batchCell{MetricID: c.MetricID, DimCodes: c.Members, Text: c.Text, Member: c.Member, Clear: c.Clear}
		if c.Value != nil {
			bc.Value = *c.Value
		}
		body.Cells = append(body.Cells, bc)
	}

	req := p.request("/api/cells/batch", nil)
	req.Method = http.MethodPost
	req.Body = body
	resp, err := w.Write(ctx, s.subject, s.client, req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &Error{Code: CodeUnavailable, Message: "the write took too long and was stopped; read the cells to see whether it was written before trying again"}
		}
		return nil, errUnavailable
	}
	var answer struct {
		RevisionID string `json:"revision_id"`
		Cells      int    `json:"cells"`
		Cleared    int    `json:"cleared"`
		Error      string `json:"error"`
		Refused    []struct {
			Index    int    `json:"index"`
			MetricID string `json:"metric_id"`
			Error    string `json:"error"`
		} `json:"refused"`
	}
	_ = json.Unmarshal(resp.Body, &answer)
	switch resp.Status {
	case http.StatusOK:
		if answer.RevisionID != p.RevisionID {
			return nil, errUnavailable
		}
		res := &WriteResult{Context: p, Status: "written", Cells: answer.Cells, Cleared: answer.Cleared,
			Note: "Written as you, in the active revision, and recalculated: query_grid reads the new values. The change is in the cell history and the audit log."}
		if dryRun {
			res.Status = "valid"
			res.Note = "Every cell can be written; nothing was written. Call again without dry_run to write them."
		}
		return res, nil
	case http.StatusUnprocessableEntity:
		parts := make([]string, 0, len(answer.Refused))
		for _, r := range answer.Refused {
			parts = append(parts, fmt.Sprintf("cell %d (metric %s): %s", r.Index, r.MetricID, r.Error))
		}
		return nil, &Error{Code: CodeRefused, Message: "nothing was written — " + strings.Join(parts, "; ")}
	case http.StatusForbidden, http.StatusNotFound:
		return nil, &Error{Code: CodeRefused, Message: "nothing was written — " + routeMessage(resp.Body)}
	case http.StatusUnauthorized:
		return nil, &Error{Code: CodeUnauthorized, Message: "your account is not active in maverickbuilds.app or no longer signs in; reconnect"}
	case http.StatusPaymentRequired:
		return nil, &Error{Code: CodeRefused, Message: "nothing was written — " + routeMessage(resp.Body)}
	case http.StatusBadRequest:
		return nil, invalid("%s", routeMessage(resp.Body))
	default:
		return nil, errUnavailable
	}
}
