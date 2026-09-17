package clients

import (
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

// dialOption returns the transport credentials for service-to-service dials:
// mTLS when the MTLS_* env vars are set (see pkg/grpcutil/tls.go), plaintext
// otherwise. A half-configured mTLS setup errors instead of silently falling
// back to plaintext.
func dialOption() (grpc.DialOption, error) {
	if opt, enabled, err := grpcutil.ClientTLSOption(); err != nil {
		return nil, fmt.Errorf("mTLS client config: %w", err)
	} else if enabled {
		return opt, nil
	}
	return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
}
