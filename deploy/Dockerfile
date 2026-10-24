# syntax=docker/dockerfile:1
# Base images are pinned by digest; .claude/skills/maintenance/SKILL.md updates them.
FROM golang:1.27-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X kazari/internal/buildinfo.version=${VERSION} -X kazari/internal/buildinfo.commit=${COMMIT} -X kazari/internal/buildinfo.date=${DATE}" \
    -o /out/kazari ./cmd/kazari
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/kazari /usr/local/bin/kazari
# An empty data directory owned by nonroot (65532): Docker initialises a new
# named volume mounted here from the image, so the server can write to it.
COPY --from=build --chown=65532:65532 /out/data /var/lib/kazari
VOLUME /var/lib/kazari
EXPOSE 80 443
ENV KAZARI_DATA_DIR=/var/lib/kazari
ENTRYPOINT ["/usr/local/bin/kazari"]
CMD ["serve", "--config", "/etc/kazari/kazari.yaml"]
