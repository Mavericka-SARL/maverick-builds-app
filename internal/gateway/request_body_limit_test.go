package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The cap ingress-nginx used to enforce now lives in the gateway: a body over
// maxRequestBodyBytes fails when read, whatever proxy is in front, and one at
// the limit still reads whole.
func TestRequestBodiesAreCappedByTheGateway(t *testing.T) {
	var readErr error
	var readLen int
	h := limitRequestBodies(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		readLen, readErr = len(b), err
	}))

	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"at the limit", maxRequestBodyBytes, false},
		{"one byte over", maxRequestBodyBytes + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/anything", bytes.NewReader(make([]byte, tc.size)))
			h.ServeHTTP(httptest.NewRecorder(), req)
			var tooLarge *http.MaxBytesError
			switch {
			case tc.wantErr && !errors.As(readErr, &tooLarge):
				t.Fatalf("reading %d bytes: err = %v, want *http.MaxBytesError", tc.size, readErr)
			case !tc.wantErr && (readErr != nil || readLen != tc.size):
				t.Fatalf("reading %d bytes: got %d, err = %v", tc.size, readLen, readErr)
			}
		})
	}
}
