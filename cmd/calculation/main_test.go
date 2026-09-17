package main

import (
	"testing"

	calculationv1 "github.com/mavericks-engine/mavericks/gen/go/calculation/v1"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

func TestAuditPolicyComplete(t *testing.T) {
	grpcutil.AssertAuditPolicyComplete(t, calculationv1.CalculationService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
}
