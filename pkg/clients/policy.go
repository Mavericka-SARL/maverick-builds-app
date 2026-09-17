package clients

import (
	"fmt"

	"google.golang.org/grpc"

	policyv1 "github.com/mavericks-engine/mavericks/gen/go/policy/v1"
)

// NewPolicyClient dials the Policy Service and returns a typed gRPC client.
func NewPolicyClient(addr string) (policyv1.PolicyServiceClient, *grpc.ClientConn, error) {
	creds, err := dialOption()
	if err != nil {
		return nil, nil, err
	}
	conn, err := grpc.NewClient(addr, creds)
	if err != nil {
		return nil, nil, fmt.Errorf("dial policy service %s: %w", addr, err)
	}
	return policyv1.NewPolicyServiceClient(conn), conn, nil
}

// NewCalculationClient dials the Calculation Service.
func NewCalculationClient(addr string) (interface{}, *grpc.ClientConn, error) {
	creds, err := dialOption()
	if err != nil {
		return nil, nil, err
	}
	conn, err := grpc.NewClient(addr, creds)
	if err != nil {
		return nil, nil, fmt.Errorf("dial calculation service %s: %w", addr, err)
	}
	return conn, conn, nil
}
