package main

import (
	"testing"

	notificationv1 "github.com/mavericks-engine/mavericks/gen/go/notification/v1"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
)

func TestAuditPolicyComplete(t *testing.T) {
	grpcutil.AssertAuditPolicyComplete(t, notificationv1.NotificationService_ServiceDesc.Methods, auditPolicy, deliberatelyUnaudited)
}
