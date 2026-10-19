ARG PG_MAJOR=16
# postgis/postgis is published for linux/amd64 only, so PostGIS is installed
# from the PGDG apt repository already configured in the official postgres
# image. That keeps the image multi-arch (amd64 and arm64) like cvctl-postgres.
FROM postgres:${PG_MAJOR}-bookworm

ARG PG_MAJOR
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends "postgresql-${PG_MAJOR}-postgis-3" "postgresql-${PG_MAJOR}-postgis-3-scripts"; \
    rm -rf /var/lib/apt/lists/*

ARG TARGETARCH
ARG WALG_VERSION=v3.0.9
# Pinned here rather than read from the release's .sha256 next to the asset:
# a checksum from the same origin cannot detect a replaced asset. Update both
# with WALG_VERSION.
ARG WALG_SHA256_AMD64=4f03ee4679db7f660bfd1b2b8291ac5df247cd2ec48074b60c112f4897299f24
ARG WALG_SHA256_ARM64=20191a37ad091a880c25f4cc907e6f0deb7f12bdd8a53fd62dbc7fcbf2eac712

# curl is purged in the same layer so it does not stay in the final image.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) arch=amd64; sum="${WALG_SHA256_AMD64}" ;; \
      arm64) arch=aarch64; sum="${WALG_SHA256_ARM64}" ;; \
      *) echo "unsupported arch: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    asset="wal-g-pg-22.04-${arch}.tar.gz"; \
    apt-get update; \
    apt-get install -y --no-install-recommends curl ca-certificates; \
    cd /tmp; \
    curl -fsSLO "https://github.com/wal-g/wal-g/releases/download/${WALG_VERSION}/${asset}"; \
    echo "${sum}  ${asset}" | sha256sum -c -; \
    tar -xzf "${asset}"; \
    install -m 0755 "wal-g-pg-22.04-${arch}" /usr/local/bin/wal-g; \
    rm -f "${asset}" "wal-g-pg-22.04-${arch}"; \
    apt-get purge -y --auto-remove curl; \
    rm -rf /var/lib/apt/lists/*
