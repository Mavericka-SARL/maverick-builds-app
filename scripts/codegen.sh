#!/usr/bin/env bash
# Run all code generators: buf (proto), ogen (OpenAPI).
# Run from the repo root.
set -euo pipefail

echo "==> buf generate (protobuf)"
buf generate

echo "==> ogen (OpenAPI → Go)"
ogen --target internal/gateway/oas --package oas --clean api/openapi.yaml

echo "==> done"
