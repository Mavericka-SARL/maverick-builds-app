// Package infra is the cluster's own furniture — Postgres, PgBouncer, NATS,
// Redis, MinIO, Keycloak — as manifests, plus the tests that have anything to
// check about them: the Keycloak login theme and the Keycloak realm are each
// kept twice, once as real files the Compose stacks mount and once inlined in
// a ConfigMap the cluster mounts, and the copies have to agree.
package infra
