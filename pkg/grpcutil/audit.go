package grpcutil

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
	"github.com/mavericks-engine/mavericks/pkg/auth"
)

// AuditPolicy maps a service's RPC bare method names (grpc.MethodDesc.MethodName,
// e.g. "CreateCustomer" — NOT the full "/tenant.v1.TenantService/CreateCustomer"
// path) to the category each should be recorded under. Every RPC a service's
// generated ServiceDesc.Methods lists must appear either here or in that
// service's own deliberatelyUnaudited list — enforced per-service by
// AssertAuditPolicyComplete (audit_testutil.go) in each service's own
// main_test.go, not centrally, matching this file's existing "audit wiring
// lives in the service's own main.go" convention.
//
// Replaces an earlier prefix-matching heuristic (isMutation, matching
// method names starting with Create/Update/Delete/Upsert/Assign/Revoke/
// Apply/Rollback/Publish/Submit/Mark): that approach silently missed any
// RPC whose name didn't happen to start with one of those words, with
// nothing to catch the omission — found live, 2026-08-10, 12 real
// mutating RPCs across 8 services (e.g. query.Writeback,
// importpkg.CommitImport) had zero audit trail as a result. Explicit
// per-RPC classification, enforced by a test that reads the real
// generated method list, makes that specific failure mode structurally
// impossible to repeat.
type AuditPolicy map[string]auditv1.EventCategory

// bareMethodName extracts the method name from a full gRPC method path,
// e.g. "/tenant.v1.TenantService/CreateCustomer" -> "CreateCustomer" —
// matches grpc.MethodDesc.MethodName's form, which is what AuditPolicy is
// keyed by.
func bareMethodName(fullMethod string) string {
	parts := strings.SplitN(fullMethod, "/", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// WithAudit returns the ServerOption that installs AuditInterceptor, or nil
// if client is nil (e.g. because dialing the audit service failed at
// startup) — callers can unconditionally splat the result into
// NewServer(...) without their own nil check.
func WithAudit(client auditv1.AuditServiceClient, policy AuditPolicy, log zerolog.Logger) []grpc.ServerOption {
	if client == nil {
		return nil
	}
	return []grpc.ServerOption{grpc.ChainUnaryInterceptor(AuditInterceptor(client, policy, log))}
}

// AuditInterceptor returns a unary server interceptor that records an audit
// event for every successful RPC policy declares as audited. Events are
// fired in the background so they never block the RPC response.
func AuditInterceptor(client auditv1.AuditServiceClient, policy AuditPolicy, log zerolog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			return resp, err
		}
		category, audited := policy[bareMethodName(info.FullMethod)]
		if !audited {
			return resp, nil
		}

		actor, _ := auth.ActorFromContext(ctx)

		// Build a compact payload from the request proto if possible
		var payload []byte
		if pm, ok := req.(proto.Message); ok {
			payload, _ = protojson.Marshal(pm)
		}

		go func() { //nolint:contextcheck
			aCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			event := &auditv1.AuditEvent{
				Category:   category,
				EventType:  methodToEventType(info.FullMethod),
				AfterState: payload,
			}
			if actor != nil {
				event.Actor = actor
			}

			if _, aerr := client.RecordEvent(aCtx, &auditv1.RecordEventRequest{Event: event}); aerr != nil {
				log.Warn().Err(aerr).Str("method", info.FullMethod).Msg("audit record failed")
			}
		}()

		return resp, nil
	}
}

// methodToEventType converts "/service.v1.FooService/CreateBar" → "foo.create_bar".
func methodToEventType(fullMethod string) string {
	parts := strings.SplitN(fullMethod, "/", 3)
	if len(parts) < 3 {
		return fullMethod
	}
	// package: e.g. "tenant.v1.TenantService" → take first segment "tenant"
	svcParts := strings.Split(parts[1], ".")
	svc := "unknown"
	if len(svcParts) > 0 {
		svc = svcParts[0]
	}
	// method: CamelCase → snake_case
	return svc + "." + camelToSnake(parts[2])
}

func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' && i > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(r | 0x20) // to lower
	}
	return b.String()
}
