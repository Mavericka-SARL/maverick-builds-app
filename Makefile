SHELL := bash
PATH  := $(shell brew --prefix 2>/dev/null || echo /usr/local)/bin:$(PATH)

COMPOSE := docker compose -f deploy/docker/docker-compose.dev.yml
SERVICES := gateway integration

.PHONY: proto oas gen build test lint clean dev-up dev-down dev-admin dev-logs obs-up obs-down demo $(SERVICES:%=build-%)

# ── Protobuf ───────────────────────────────────────────────────────────────────

proto:
	buf generate

lint-proto:
	buf lint

# ── OpenAPI ───────────────────────────────────────────────────────────────────

# Regenerate internal/gateway/oas from api/openapi.yaml. RUN THIS AFTER EVERY
# EDIT TO api/openapi.yaml and commit the result alongside the spec: CI's
# "ogen generate (verify no drift)" job regenerates with this exact pinned
# version and fails on any diff — including one caused purely by a changed
# summary or description, which ogen bakes into the generated router and
# handler comments.
OGEN_VERSION := v1.20.3

oas:
	go run github.com/ogen-go/ogen/cmd/ogen@$(OGEN_VERSION) \
		--target internal/gateway/oas --package oas --clean api/openapi.yaml

# Everything CI checks for drift. Cheaper than discovering it in CI.
gen: proto oas

# ── Go ────────────────────────────────────────────────────────────────────────

build:
	go build ./...

test:
	go test ./...

lint:
	golangci-lint run ./...

$(SERVICES:%=build-%):
	go build -o bin/$(@:build-%=%) ./cmd/$(@:build-%=%)

build-all: $(SERVICES:%=build-%)

clean:
	rm -rf bin/

# ── Core dev environment ───────────────────────────────────────────────────────

dev-up:
	$(COMPOSE) up -d
	@echo ""
	@echo "  Services:"
	@echo "    Postgres    → localhost:5432   (user: mavericks / pw: mavericks)"
	@echo "    PgBouncer   → localhost:5433   (pool mode: transaction)"
	@echo "    Keycloak    → http://localhost:8180  (admin/admin, realm: mavericks)"
	@echo "    MinIO       → http://localhost:9001  (mavericks / mavericks123)"
	@echo ""

# The platform_admin dev persona (X-Dev-User: platform_admin), which a fresh
# database lacks; the seeds start as it. Run once, after the gateway's first
# start has migrated the database.
dev-admin:
	SUBJECT=demo-platform-admin-001 ADMIN_EMAIL=platform.admin@example.com ADMIN_NAME="Platform Admin" \
	PSQL="$(COMPOSE) exec -T postgres psql -q -U mavericks -d $${DEV_DB:-mavericks}" \
	bash scripts/bootstrap-platform-admin.sh

dev-down:
	$(COMPOSE) --profile observability down

dev-logs:
	$(COMPOSE) logs -f

dev-ps:
	$(COMPOSE) ps

# ── Observability stack (Prometheus · Loki · Tempo · Grafana · OTel Collector) ─

obs-up:
	$(COMPOSE) --profile observability up -d
	@echo ""
	@echo "  Observability:"
	@echo "    Grafana     → http://localhost:3000  (anonymous admin)"
	@echo "    Prometheus  → http://localhost:9090"
	@echo "    Loki        → http://localhost:3100"
	@echo "    Tempo       → http://localhost:3200"
	@echo "    OTel        → localhost:4317 (gRPC)  localhost:4318 (HTTP)"
	@echo ""

obs-down:
	$(COMPOSE) --profile observability stop otel-collector prometheus loki tempo grafana

# ── Demo seed ──────────────────────────────────────────────────────────────────
# Builds the reference demo through the running gateway's HTTP API, as a real
# developer would (start it first: bash dev.sh).

demo:
	go run ./cmd/seed-regional-planning/
