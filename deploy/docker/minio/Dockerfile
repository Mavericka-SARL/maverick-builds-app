# MinIO server, built from source.
#
# MinIO no longer publishes an image anyone can pull: Docker Hub's minio/minio
# vanished on 2026-09-11, and quay.io/minio has answered every anonymous pull
# with 401 since 2026-09-24 — the same withdrawal that made
# deploy/docker/pg-backup build mc from source. The source is still public, so
# this builds the release deploy/k8s/base/infra/minio.yaml was pinned to.
#
#   docker build -t mavericks-minio deploy/docker/minio
#
# The self-hosted Compose stack (deploy/compose) builds it for the object store
# behind package export and the nightly database dumps.
FROM golang:1.25 AS build
ARG MINIO_VERSION=RELEASE.2025-09-07T16-13-09Z
RUN git clone --depth 1 --branch ${MINIO_VERSION} https://github.com/minio/minio.git /src
WORKDIR /src
# The version variables MinIO's own release build stamps; without them the
# server calls itself DEVELOPMENT.GOGET. Version is the tag's timestamp in
# RFC 3339 (RELEASE.2025-09-07T16-13-09Z → 2025-09-07T16:13:09Z).
RUN version="$(echo "${MINIO_VERSION#RELEASE.}" | sed -E 's/T([0-9]{2})-([0-9]{2})-([0-9]{2})Z$/T\1:\2:\3Z/')" \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -tags kqueue \
      -ldflags="-s -w -X github.com/minio/minio/cmd.Version=${version} -X github.com/minio/minio/cmd.ReleaseTag=${MINIO_VERSION} -X github.com/minio/minio/cmd.CommitID=$(git rev-parse HEAD) -X github.com/minio/minio/cmd.ShortCommitID=$(git rev-parse --short=12 HEAD)" \
      -o /out/minio .

# Alpine rather than distroless: the Compose health check needs wget.
FROM alpine:3.22
COPY --from=build /out/minio /usr/bin/minio
EXPOSE 9000 9001
ENTRYPOINT ["minio"]
CMD ["server", "/data", "--console-address", ":9001"]
