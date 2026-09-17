package main

import (
	"testing"

	queryv1 "github.com/mavericks-engine/mavericks/gen/go/query/v1"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

func TestAuditPolicyComplete(t *testing.T) {
	grpcutil.AssertAuditPolicyComplete(t, queryv1.QueryService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
}
