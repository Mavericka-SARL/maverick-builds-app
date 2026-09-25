// Package testobjectstore starts MinIO for tests that need real object
// storage, from an image built out of deploy/docker/minio — the Dockerfile
// the self-hosted Compose stack builds.
//
// MinIO no longer publishes an image anyone can pull. Docker Hub's
// minio/minio vanished on 2026-09-11, and quay.io/minio has refused anonymous
// pulls since 2026-09-24, which failed every object-storage test on a runner
// that did not already have the old image cached — first CI's GitHub-hosted
// runners, eventually everyone.
//
// The image is built once per test binary and kept (KeepImage), so Docker's
// build cache survives between runs: from source the first build takes
// minutes, every later one seconds. CI runs this package's own test on its
// own before the suite, so that one slow build never counts against another
// package's test timeout.
package testobjectstore

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
)

const (
	repo = "mavericks-test-minio"
	tag  = "local"
)

var (
	buildOnce sync.Once
	buildErr  error
)

// Run starts a MinIO server for the calling test and terminates it when the
// test ends. Every call gets a server of its own, as tests had when each
// started a published image: no bucket or object is shared between tests.
func Run(t *testing.T) *tcminio.MinioContainer {
	t.Helper()
	ctx := context.Background()

	buildOnce.Do(func() { buildErr = build(ctx) })
	if buildErr != nil {
		t.Fatalf("build the MinIO test image: %v", buildErr)
	}

	ctr, err := tcminio.Run(ctx, repo+":"+tag)
	// Terminated per test rather than shared: CI runs with Ryuk disabled, so
	// a container nobody terminates outlives the run.
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}
	return ctr
}

// build builds the image without starting anything. The build log is kept
// only to explain a failure: a compiler error inside a Dockerfile is useless
// as "build failed".
func build(ctx context.Context) error {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	defer provider.Close()

	var log bytes.Buffer
	req := &testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:        dockerfileDir(),
			Repo:           repo,
			Tag:            tag,
			KeepImage:      true,
			BuildLogWriter: &log,
		},
	}
	if _, err := provider.BuildImage(ctx, req); err != nil {
		return fmt.Errorf("%w\n%s", err, lastLines(log.String(), 30))
	}
	return nil
}

// dockerfileDir finds deploy/docker/minio from this file rather than from the
// working directory, which is whichever package's test is running.
func dockerfileDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy", "docker", "minio")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
