# syntax=docker/dockerfile:1
# check=error=true
# Digest-pinned; the maintenance skill updates them.
# The build stage runs on the builder's platform and cross-compiles, so another target needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
# The source is bind-mounted rather than copied into a layer, and the module and build caches
# outlive the build, so a rebuild compiles only what changed.
RUN --mount=type=bind,target=. \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/PeterBooker/kazari/internal/buildinfo.version=${VERSION} -X github.com/PeterBooker/kazari/internal/buildinfo.commit=${COMMIT} -X github.com/PeterBooker/kazari/internal/buildinfo.date=${DATE}" \
    -o /out/kazari ./cmd/kazari
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /out/kazari /usr/local/bin/kazari
# Owned by nonroot (65532): a new named volume copies it, so the server can write.
COPY --from=build --chown=65532:65532 /out/data /var/lib/kazari
# The nonroot base sets this too; stated so that another base tag cannot run as root.
USER 65532:65532
VOLUME /var/lib/kazari
EXPOSE 80 443
ENV KAZARI_DATA_DIR=/var/lib/kazari
# Over the admin socket in the data volume, which needs no token, and with no shell.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --start-interval=1s --retries=3 \
    CMD ["/usr/local/bin/kazari", "admin", "health"]
ENTRYPOINT ["/usr/local/bin/kazari"]
# serve reads /etc/kazari/kazari.yaml when it is mounted, else the KAZARI_* environment.
CMD ["serve"]
