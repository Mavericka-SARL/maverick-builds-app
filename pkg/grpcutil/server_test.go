package grpcutil_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

// TestNewServer_RegistersHealthService is a regression test for the exact
// bug this batch fixes: every service manifest under
// deploy/k8s/base/services/ probes readiness/liveness via
// `grpc-health-probe -addr=:9090` (no -service flag, i.e. the empty/overall
// service name), which requires the standard grpc.health.v1 Health service
// to be registered. Before this fix, NewServer() never registered it, so
// every real deployment's probes would fail with NOT_FOUND and pods would
// never become ready.
func TestNewServer_RegistersHealthService(t *testing.T) {
	srv := grpcutil.NewServer()

	lc := &net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := healthgrpc.NewHealthClient(conn)
	// Empty Service name matches what `grpc-health-probe -addr=:PORT` (no
	// -service flag) checks in every real probe.
	resp, err := client.Check(context.Background(), &healthgrpc.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.Status != healthgrpc.HealthCheckResponse_SERVING {
		t.Errorf("status = %v, want SERVING", resp.Status)
	}
}
