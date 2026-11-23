# syntax=docker/dockerfile:1
# Digest-pinned; the maintenance skill updates them.
FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/PeterBooker/kazari/internal/buildinfo.version=${VERSION} -X github.com/PeterBooker/kazari/internal/buildinfo.commit=${COMMIT} -X github.com/PeterBooker/kazari/internal/buildinfo.date=${DATE}" \
    -o /out/kazari ./cmd/kazari
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /out/kazari /usr/local/bin/kazari
# Owned by nonroot (65532): a new named volume copies it, so the server can write.
COPY --from=build --chown=65532:65532 /out/data /var/lib/kazari
VOLUME /var/lib/kazari
EXPOSE 80 443
ENV KAZARI_DATA_DIR=/var/lib/kazari
ENTRYPOINT ["/usr/local/bin/kazari"]
# serve reads /etc/kazari/kazari.yaml when it is mounted, else the KAZARI_* environment.
CMD ["serve"]
