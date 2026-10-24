# Go toolchain

## Check

```sh
go version
grep '^go ' go.mod
grep '^FROM golang' deploy/Dockerfile
curl -fsSL 'https://go.dev/dl/?mode=json' | jq -r '.[0].version'
```

A standard library finding from govulncheck is fixed by moving to the Go release that patches it.

## Update

```sh
go get go@1.27.4      # updates the go line; do not add a toolchain line
go mod tidy
```

Every release, patch or minor, also refreshes the `golang` image digest (`docker.md`): the digest is how a patch release reaches the image.

A minor release (1.27 to 1.28) also:

- changes `golang:1.27-alpine` to `golang:1.28-alpine` in `deploy/Dockerfile` and "Go (1.27 or later)" in `site/content/docs/quickstart.md`;
- needs the release notes read (<https://go.dev/doc/devel/release>). Raising the `go` line switches `GODEBUG` defaults (listed in the notes' compatibility section), which can change `net/http`, `crypto/tls` and `crypto/x509`. Look at what affects the TLS listener and CertMagic, the `ServeMux` host patterns, the OAuth and OIDC HTTP clients and `archive/tar`;
- can bring new `go vet` findings: fix them rather than silencing them.

## Verify

`SKILL.md`'s verification, plus `docker.md`'s for the image.
