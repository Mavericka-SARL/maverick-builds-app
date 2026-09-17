package clients

import (
	"fmt"

	"google.golang.org/grpc"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
)

// NewAuditClient dials the Audit Service and returns a typed gRPC client.
func NewAuditClient(addr string) (auditv1.AuditServiceClient, *grpc.ClientConn, error) {
	creds, err := dialOption()
	if err != nil {
		return nil, nil, err
	}
	conn, err := grpc.NewClient(addr, creds)
	if err != nil {
		return nil, nil, fmt.Errorf("dial audit service %s: %w", addr, err)
	}
	return auditv1.NewAuditServiceClient(conn), conn, nil
}
