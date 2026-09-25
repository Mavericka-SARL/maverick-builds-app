package testobjectstore_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testobjectstore"
)

// The image builds and serves S3. CI runs this test alone before the suite
// (.github/workflows/ci.yml), which is what makes the first, slow build happen
// outside every other package's test timeout.
func TestRunServesObjectStorage(t *testing.T) {
	ctx := context.Background()
	ctr := testobjectstore.Run(t)

	endpoint, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+endpoint+"/minio/health/ready", nil)
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %s, want 200", resp.Status)
	}
	if ctr.Username == "" || ctr.Password == "" {
		t.Fatalf("no credentials reported (user %q)", ctr.Username)
	}
}
