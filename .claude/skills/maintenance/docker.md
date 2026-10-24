# Docker images

| Image | In | Rule |
| ----- | -- | ---- |
| `golang:<minor>-alpine` | `deploy/Dockerfile` | the `go.mod` minor |
| `gcr.io/distroless/static-debianNN:nonroot` | both Dockerfiles | same image and digest in both |
| `busybox:<minor>` | `deploy/Dockerfile.goreleaser` | the latest minor; it only creates an empty data directory |

## Check

What the Dockerfiles use, the newest Go alpine and busybox minor tags on Docker Hub, and whether distroless publishes the next Debian release (use the current number plus one; prints `true` or `false`):

```sh
grep -n '^FROM' deploy/Dockerfile deploy/Dockerfile.goreleaser
curl -fsSL 'https://hub.docker.com/v2/repositories/library/golang/tags?page_size=100&name=-alpine' \
  | jq -r '.results[].name' | grep -E '^[0-9]+\.[0-9]+-alpine$' | sort -V | tail -n1
curl -fsSL 'https://hub.docker.com/v2/repositories/library/busybox/tags?page_size=100' \
  | jq -r '.results[].name' | grep -E '^[0-9]+\.[0-9]+$' | sort -V | tail -n1
curl -fsSL https://gcr.io/v2/distroless/static-debian13/tags/list | jq '.tags | index("nonroot") != null'
```

The registry answers a Debian release it does not have with an empty tag list rather than an error, so check for the `nonroot` tag; a status code check would say yes.

## Update

Every `FROM` is pinned by the digest of the multi-platform index, so a tag that moves upstream cannot change what is built. Refresh every digest whenever you touch the images (after any tag change):

```sh
for img in $(sed -n 's/^FROM \(--platform=[^ ]* \)\{0,1\}\([^@ ]*\)@.*/\2/p' deploy/Dockerfile deploy/Dockerfile.goreleaser | sort -u); do
  echo "$img@$(docker buildx imagetools inspect "$img" --format '{{json .Manifest.Digest}}' | tr -d '"')"
done
```

Write each `<image>:<tag>@sha256:...` back into its `FROM` line.

## Verify

```sh
docker build -f deploy/Dockerfile -t kazari:check .
docker compose up --build -d && docker compose ps                # must report healthy
goreleaser release --snapshot --clean --skip=publish,sign,sbom   # builds the Dockerfile.goreleaser images
```

`docker compose up --build` rebuilds and restarts the user's local development server (its data stays in the `kazari_data` volume). If `docker compose ps` shows it running, say so before you run it.
