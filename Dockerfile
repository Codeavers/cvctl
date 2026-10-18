# Release image: static binaries are built by release.yml, no Go toolchain here.
FROM scratch
ARG TARGETARCH
COPY dist/cvctl-linux-${TARGETARCH} /cvctl
ENTRYPOINT ["/cvctl"]
