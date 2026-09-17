package main

import (
	"testing"

	schemamigrationv1 "github.com/mavericks-engine/mavericks/gen/go/schemamigration/v1"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

func TestAuditPolicyComplete(t *testing.T) {
	grpcutil.AssertAuditPolicyComplete(t, schemamigrationv1.SchemaMigrationService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
}
