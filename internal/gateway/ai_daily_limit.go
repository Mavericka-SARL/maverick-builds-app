package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// maxDailyCallLimit bounds the daily call limit a developer or tenant admin
// may set: far above any day's build, below an overflow.
const maxDailyCallLimit = 1_000_000

// optionalLimit is a daily call limit in a request body: set says the field
// came at all (absent keeps the saved limit), and a null or 0 clears it.
type optionalLimit struct {
	set bool
	n   *int
}

func (o *optionalLimit) UnmarshalJSON(b []byte) error {
	o.set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.n = nil
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("daily_call_limit: a whole number of calls, or null for none")
	}
	if n < 0 || n > maxDailyCallLimit {
		return fmt.Errorf("daily_call_limit: between 1 and %d calls, or null for none", maxDailyCallLimit)
	}
	if n == 0 {
		o.n = nil
		return nil
	}
	o.n = &n
	return nil
}

// limitWord is a limit for an audit record: "none" when cleared.
func limitWord(n *int) string {
	if n == nil {
		return "none"
	}
	return fmt.Sprint(*n)
}
