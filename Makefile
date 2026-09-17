SHELL := bash
PATH  := $(shell brew --prefix 2>/dev/null || echo /usr/local)/bin:$(PATH)

COMPOSE := docker compose -f deploy/docker/docker-compose.dev.yml
SERVICES := gateway identity tenant model schema-migration policy calculation workflow import query audit notification ai-assistant integration seed

.PHONY: proto oas sqlc gen build test lint clean dev-up dev-down dev-logs obs-up obs-down demo $(SERVICES:%=build-%)

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

# Regenerate the sqlc models from migrations/. RUN THIS AFTER ADDING A
# MIGRATION: CI's "sqlc generate (verify no drift)" job regenerates and fails
# on any diff, and a migration that adds a column changes the generated
# structs in internal/*/db/models.go even when no query mentions it.
SQLC_VERSION := v1.31.1

sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

# Everything CI checks for drift. Cheaper than discovering it in CI.
gen: proto oas sqlc

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
	@echo "    NATS        → localhost:4222   (JetStream enabled)"
	@echo "    Redis       → localhost:6379"
	@echo "    Keycloak    → http://localhost:8180  (admin/admin, realm: mavericks)"
	@echo "    MinIO       → http://localhost:9001  (mavericks / mavericks123)"
	@echo ""

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

demo:
	docker exec docker-postgres-1 psql -U mavericks -d postgres -c "DROP DATABASE IF EXISTS mavericks WITH (FORCE);" 2>/dev/null || true
	docker exec docker-postgres-1 psql -U mavericks -d postgres -c "CREATE DATABASE mavericks;" 2>/dev/null || true
	go run ./cmd/seed/

demo-budget:
	go run ./cmd/seed-budget/
