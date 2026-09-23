// Package infra is the cluster's own furniture — Postgres, PgBouncer, NATS,
// Redis, MinIO, Keycloak — as manifests, plus the one test that has anything
// to check about them: the Keycloak login theme is kept twice, once as real
// files a developer's stack mounts and once inlined in a ConfigMap the
// cluster mounts, and the copies have to agree.
package infra
