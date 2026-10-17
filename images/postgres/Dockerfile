ARG PG_MAJOR=16
FROM postgres:${PG_MAJOR}-bookworm

ARG TARGETARCH
ARG WALG_VERSION=v3.0.9

# Checksum is verified from the .sha256 published next to the asset; curl is
# purged in the same layer so it does not stay in the final image.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) arch=amd64 ;; \
      arm64) arch=aarch64 ;; \
      *) echo "unsupported arch: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    asset="wal-g-pg-22.04-${arch}.tar.gz"; \
    apt-get update; \
    apt-get install -y --no-install-recommends curl ca-certificates; \
    cd /tmp; \
    curl -fsSLO "https://github.com/wal-g/wal-g/releases/download/${WALG_VERSION}/${asset}"; \
    curl -fsSLO "https://github.com/wal-g/wal-g/releases/download/${WALG_VERSION}/${asset}.sha256"; \
    sha256sum -c "${asset}.sha256"; \
    tar -xzf "${asset}"; \
    install -m 0755 "wal-g-pg-22.04-${arch}" /usr/local/bin/wal-g; \
    rm -f "${asset}" "${asset}.sha256" "wal-g-pg-22.04-${arch}"; \
    apt-get purge -y --auto-remove curl; \
    rm -rf /var/lib/apt/lists/*
