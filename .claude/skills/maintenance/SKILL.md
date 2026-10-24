---
name: maintenance
description: Update Kazari's dependencies - the Go toolchain, Go modules, Docker base images, GitHub Actions, goreleaser and Hugo. Use when asked to update, upgrade or bump any of them, to check what is out of date, or to fix a govulncheck finding.
---

# Maintenance: updating dependencies

A request that names one area ("bump Hugo") touches only that area; "update the dependencies" means all of them. Each area's file in this directory has its checks (they change nothing and need `curl`, `jq` and `git`), its update steps and its own verification. Read only the files you need.

| Area | File |
| ---- | ---- |
| Go toolchain; a govulncheck finding in the standard library | `go.md` |
| Go modules; a govulncheck finding in a module | `modules.md` |
| Docker base images | `docker.md` |
| GitHub Actions, goreleaser | `actions.md` |
| Hugo | `hugo.md` |

staticcheck and govulncheck run at `@latest`: nothing to pin.

Before changing anything, run `go test -race ./...` once, so a failure that was already there is not blamed on an update.

## What to take

- Patch and minor updates: take them.
- GitHub Actions majors: take them after reading the release notes. Usually it is a newer Node runtime; check for renamed or removed inputs that the workflows use.
- A new Debian release of the distroless image: take it. The image is static with no libc, and `nonroot` stays UID 65532.
- A Go minor release: take it after reading the release notes (`go.md`).
- Go module majors, a goreleaser major, replacing or adding a direct dependency: not unprompted. They need code or config changes and are design decisions. Report what the change would involve, and ask.

## Verify

The CI checks and `go test -race ./...` from CLAUDE.md, then the builds of the other CI jobs (`ci.yml` build, `pages.yml`):

```sh
for p in linux/amd64 linux/arm64 darwin/arm64; do
  GOOS=${p%/*} GOARCH=${p#*/} CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/kazari
done
hugo --source site --gc --minify
```

Then run each changed area's checks again: everything you meant to take should show as current.

## Report and commit

Report per area: old and new versions, anything held back and why, and changelog items that affect Kazari.

Commit only when asked: one commit per area, after its verification passes, each message a single short sentence: "Update Go to 1.27.4", "Update Go modules", "Update Docker base images", "Update GitHub Actions", "Update Hugo to 0.168.0".
