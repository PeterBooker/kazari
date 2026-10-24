# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X agalma/internal/buildinfo.version=${VERSION} -X agalma/internal/buildinfo.commit=${COMMIT} -X agalma/internal/buildinfo.date=${DATE}" \
    -o /out/agalma ./cmd/agalma
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agalma /usr/local/bin/agalma
# An empty data directory owned by nonroot (65532): Docker initialises a new
# named volume mounted here from the image, so the server can write to it.
COPY --from=build --chown=65532:65532 /out/data /var/lib/agalma
VOLUME /var/lib/agalma
EXPOSE 80 443
ENV AGALMA_DATA_DIR=/var/lib/agalma
ENTRYPOINT ["/usr/local/bin/agalma"]
CMD ["serve", "--config", "/etc/agalma/agalma.yaml"]
