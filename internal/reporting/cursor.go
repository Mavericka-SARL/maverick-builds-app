package reporting

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// cursorLifetime bounds how long a continuation stays usable.
const cursorLifetime = 15 * time.Minute

// cursorCodec seals continuations. A cursor names a place in one query's
// results for one subject; it is signed so it cannot be edited into another
// query's or another person's, and it grants nothing: the next page is read
// as whoever presents it, under their access at that moment. The key lives
// in memory, so a restart ends every cursor — the caller asks again.
type cursorCodec struct{ key []byte }

func newCursorCodec() *cursorCodec {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("reporting: no randomness for the cursor key: " + err.Error())
	}
	return &cursorCodec{key: key}
}

type cursorState struct {
	Subject string `json:"s"`
	Query   string `json:"q"`
	Offset  int    `json:"o,omitempty"`
	// Next is a route's own continuation (a record list's X-Next-Cursor).
	Next    string `json:"n,omitempty"`
	Expires int64  `json:"e"`
}

func (c *cursorCodec) seal(st cursorState, now time.Time) string {
	st.Expires = now.Add(cursorLifetime).Unix()
	payload, _ := json.Marshal(st)
	mac := hmac.New(sha256.New, c.key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// open returns the state of token if it was sealed here for subject and
// query and has not expired.
func (c *cursorCodec) open(token, subject, query string, now time.Time) (cursorState, error) {
	bad := invalid("the cursor is not valid for this request; run the query again without it")
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return cursorState{}, bad
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	got, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return cursorState{}, bad
	}
	mac := hmac.New(sha256.New, c.key)
	mac.Write(payload)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return cursorState{}, bad
	}
	var st cursorState
	if json.Unmarshal(payload, &st) != nil || st.Subject != subject || st.Query != query {
		return cursorState{}, bad
	}
	if now.Unix() > st.Expires {
		return cursorState{}, invalid("the cursor has expired; run the query again")
	}
	return st, nil
}

// queryKey identifies a query for cursor binding: the tool and its inputs,
// cursor excluded.
func queryKey(tool string, inputs ...any) string {
	h := sha256.New()
	h.Write([]byte(tool))
	for _, in := range inputs {
		b, _ := json.Marshal(in)
		h.Write([]byte{0})
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}
