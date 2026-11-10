# postgres-walg — postgres:16-alpine plus the wal-g binary, built from
# source (wal-g has no first-party Docker image, only unofficial
# community ones — building from a pinned git tag via the Go toolchain
# matches how this repo already trusts Go module sources over
# third-party binaries everywhere else). This becomes the actual
# database server's own image (archive_command/restore_command need the
# binary local to the postgres process) as well as the base-backup
# sidecar's image — deliberately minimal, no unrelated tooling, unlike
# deploy/docker/pg-backup/ (logical pg_dump/pg_restore backups, a
# separate, complementary mechanism).
FROM golang:1.26 AS walg-builder
ARG WALG_VERSION=v3.0.8
RUN git clone --depth 1 --branch ${WALG_VERSION} https://github.com/wal-g/wal-g.git /src
WORKDIR /src/main/pg
# wal-g's own Makefile sets this (export GOEXPERIMENT=jsonv2) — without
# it, cmd/common/internal's use of the experimental encoding/json/v2
# stdlib package fails to build ("build constraints exclude all Go
# files").
ENV GOEXPERIMENT=jsonv2
# CGO_ENABLED=0 (matches deploy/docker/Dockerfile's own convention): the
# builder stage is glibc-based (golang:1.25) but the final image is
# musl-based (postgres:16-alpine) — a CGO-linked binary built here would
# fail with a misleading "no such file or directory" at runtime (missing
# glibc dynamic loader, not actually a missing file).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /wal-g .
# wal-g is Apache-2.0: the image carries its LICENSE and the licence and
# notice files of every module it vendors, listed in modules.txt.
RUN mkdir -p /out/licenses \
 && cp /src/LICENSE* /out/licenses/ \
 && cd /src && go mod vendor \
 && cp vendor/modules.txt /out/licenses/modules.txt \
 && find vendor -type f \( -iname 'LICEN[CS]E*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) \
      -exec sh -c 'mkdir -p "/out/licenses/$(dirname "$1")" && cp "$1" "/out/licenses/$1"' _ {} \;

# mc, built from source rather than copied out of MinIO's published image.
# quay.io/minio/mc stopped answering anonymous pulls on 2026-09-24 — 401 on
# every tag, the server image with it — which is the second time MinIO has
# withdrawn a distribution channel under this repo (Docker Hub's copy
# vanished on 2026-09-11, which is why this said quay.io at all). The source
# is still public and MC_VERSION below is the same tag the published image
# carried, so the binary is the one these scripts were verified against.
# CGO_ENABLED=0 for the same reason as every other builder stage here: this
# is glibc, the final image is musl.
FROM golang:1.26 AS mc
ARG MC_VERSION=RELEASE.2025-08-13T08-35-41Z
RUN git clone --depth 1 --branch ${MC_VERSION} https://github.com/minio/mc.git /src
WORKDIR /src
# Stamp the version variables mc's own Makefile stamps, so `mc --version` in a
# running container names the release instead of "DEVELOPMENT.GOGET". These
# scripts have been caught out by a behaviour change between mc versions
# before (see pg-backup.sh on the bucket-region warning), so being able to ask
# the image which one it carries is worth two ldflags. -tags kqueue is what
# upstream's own release build uses.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -tags kqueue \
      -ldflags="-s -w -X github.com/minio/mc/cmd.ReleaseTag=${MC_VERSION} -X github.com/minio/mc/cmd.CommitID=$(git rev-parse HEAD)" \
      -o /usr/bin/mc .

# mc is AGPLv3 and this image is published, so it carries mc's licence and
# notices and its Corresponding Source: the tagged source with every module
# it links (go mod vendor) plus this Dockerfile, the whole build recipe;
# nothing is modified (same as deploy/docker/minio; docs/LICENSING.md).
RUN mkdir -p /out/licenses \
 && cp LICENSE /out/licenses/LICENSE \
 && for f in CREDITS NOTICE; do if [ -f "$f" ]; then cp "$f" /out/licenses/; fi; done \
 && go mod vendor \
 && tar -C / --exclude=src/.git -czf "/out/licenses/mc-${MC_VERSION}-source.tar.gz" src \
 && printf '%s\n' \
      "MinIO client mc ${MC_VERSION} (/usr/local/bin/mc), licensed under the GNU Affero General Public License v3 (LICENSE)." \
      "" \
      "Corresponding Source: mc-${MC_VERSION}-source.tar.gz in this directory - https://github.com/minio/mc" \
      "at tag ${MC_VERSION}, commit $(git rev-parse HEAD), unmodified, with its Go module dependencies vendored." \
      "Build recipe: build.Dockerfile in this directory (the Dockerfile this image was built from)." \
      "Mavericka does not modify mc." \
      > /out/licenses/SOURCE.txt

# pg_repack, built from its tagged source against this same base image, so
# the extension (in the server's image, deploy/docker/postgres-walg) and the
# client (in the backup image, deploy/docker/pg-backup, which runs
# pg-repack.sh) are one version and match the server's PostgreSQL. It
# rewrites a table online, where VACUUM FULL locked it for the whole rewrite
# (owner's decision I9, 2026-11-10). BSD-licensed; its licence ships in
# /licenses/pg_repack. with_llvm=no: no JIT bitcode, so no clang.
FROM postgres:16-alpine AS repack
ARG PG_REPACK_VERSION=1.5.2
RUN apk add --no-cache build-base gawk git lz4-dev zstd-dev zlib-dev readline-dev openssl-dev
RUN git clone --depth 1 --branch ver_${PG_REPACK_VERSION} https://github.com/reorg/pg_repack.git /src
WORKDIR /src
RUN make with_llvm=no && make with_llvm=no install DESTDIR=/out \
 && mkdir -p /out/licenses && cp COPYRIGHT /out/licenses/ \
 && echo "pg_repack ${PG_REPACK_VERSION} - https://github.com/reorg/pg_repack, tag ver_${PG_REPACK_VERSION}, unmodified" > /out/licenses/SOURCE.txt

FROM postgres:16-alpine

COPY --from=repack /out/usr/local/ /usr/local/
COPY --from=repack /out/licenses/ /licenses/pg_repack/

COPY --from=walg-builder /wal-g /usr/local/bin/wal-g
COPY --from=walg-builder /out/licenses/ /licenses/wal-g/
COPY --from=mc /usr/bin/mc /usr/local/bin/mc
COPY --from=mc /out/licenses/ /licenses/mc/
COPY Dockerfile /licenses/mc/build.Dockerfile
COPY basebackup-loop.sh docker-entrypoint-walg.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/basebackup-loop.sh /usr/local/bin/docker-entrypoint-walg.sh

ENTRYPOINT ["docker-entrypoint-walg.sh"]
CMD ["postgres"]
