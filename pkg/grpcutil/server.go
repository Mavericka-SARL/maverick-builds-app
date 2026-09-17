package grpcutil

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

// Serve starts a gRPC server on the given port and blocks until SIGTERM/SIGINT.
// It registers reflection for grpcurl-style introspection in non-prod environments.
func Serve(ctx context.Context, port int, srv *grpc.Server, log zerolog.Logger) error {
	lc := &net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("listen :%d: %w", port, err)
	}

	reflection.Register(srv)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Info().Int("port", port).Msg("gRPC server listening")
		if err := srv.Serve(lis); err != nil {
			log.Error().Err(err).Msg("gRPC server error")
		}
	}()

	select {
	case <-stop:
	case <-ctx.Done():
	}

	log.Info().Msg("shutting down gRPC server")
	srv.GracefulStop()
	return nil
}

// NewServer returns a gRPC server with standard interceptors and the
// standard grpc.health.v1 Health service registered and reporting SERVING
// by default — every service manifest under deploy/k8s/base/services/
// probes readiness/liveness via `grpc-health-probe -addr=:9090` (no
// -service flag, i.e. the empty/overall service name), which requires this
// registration to exist at all; without it the probe gets NOT_FOUND and the
// pod never becomes ready.
func NewServer(opts ...grpc.ServerOption) *grpc.Server {
	base := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			recoveryInterceptor,
		),
	}
	// Service-to-service mTLS when MTLS_* env vars are set (pkg/grpcutil/
	// tls.go): the transport requires a CA-signed client certificate on top
	// of the per-actor token auth the interceptors already enforce. A
	// half-configured setup panics at startup — failing loudly beats
	// silently serving plaintext.
	if creds, enabled, err := ServerTLSOption(); err != nil {
		panic("mTLS server config: " + err.Error())
	} else if enabled {
		base = append(base, creds)
	}
	srv := grpc.NewServer(append(base, opts...)...)
	healthgrpc.RegisterHealthServer(srv, health.NewServer())
	return srv
}

func recoveryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return handler(ctx, req)
}
