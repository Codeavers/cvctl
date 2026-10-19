# Release image: static binaries are built by release.yml, no Go toolchain here.
FROM scratch
ARG TARGETARCH
# publish and the registry client need TLS roots; scratch has none.
COPY dist/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY dist/cvctl-linux-${TARGETARCH} /cvctl
ENTRYPOINT ["/cvctl"]
