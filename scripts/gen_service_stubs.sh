#!/usr/bin/env bash
# Generates cmd/<svc>/main.go and internal/<svc>/server.go stubs for each service.
# Usage: ./scripts/gen_service_stubs.sh
set -euo pipefail

MODULE="github.com/mavericks-engine/mavericks"

declare -A SERVICES=(
  ["tenant"]="TenantService:tenant/v1:tenantv1"
  ["model"]="ModelService:model/v1:modelv1"
  ["schemamigration"]="SchemaMigrationService:schemamigration/v1:schemamigrationv1"
  ["policy"]="PolicyService:policy/v1:policyv1"
  ["calculation"]="CalculationService:calculation/v1:calculationv1"
  ["workflow"]="WorkflowService:workflow/v1:workflowv1"
  ["importpkg"]="ImportService:importpkg/v1:importpkgv1"
  ["query"]="QueryService:query/v1:queryv1"
  ["audit"]="AuditService:audit/v1:auditv1"
  ["notification"]="NotificationService:notification/v1:notificationv1"
  ["aiassistant"]="AIAssistantService:aiassistant/v1:aiassistantv1"
  ["deployment"]="DeploymentService:deployment/v1:deploymentv1"
)

for svc in "${!SERVICES[@]}"; do
  IFS=':' read -r service_name proto_path pkg_alias <<< "${SERVICES[$svc]}"
  cmd_dir="cmd/${svc}"
  internal_dir="internal/${svc}"
  mkdir -p "$cmd_dir" "$internal_dir"

  # cmd/<svc>/main.go
  cat > "$cmd_dir/main.go" <<GOFILE
package main

import (
	"context"
	"os"

	${pkg_alias} "${MODULE}/gen/go/${proto_path}"
	"${MODULE}/internal/${svc}"
	"${MODULE}/pkg/config"
	"${MODULE}/pkg/grpcutil"
	"${MODULE}/pkg/logger"
)

type cfg struct {
	config.BaseConfig
}

func main() {
	log := logger.New("${svc}")

	var c cfg
	if err := config.Load(&c); err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
		os.Exit(1)
	}

	srv := grpcutil.NewServer()
	${pkg_alias}.Register${service_name}Server(srv, ${svc}.NewServer(log))

	if err := grpcutil.Serve(context.Background(), c.GRPCPort, srv, log); err != nil {
		log.Fatal().Err(err).Msg("server exited with error")
	}
}
GOFILE

  echo "wrote $cmd_dir/main.go"
done

echo "done"
