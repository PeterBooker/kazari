---
title: Publish from code
description: Publish to Kazari from a terminal, a script or CI with the kazari CLI and the REST API, including preview deployments that expire on their own.
---

# Publish from code

Everything an agent can do through the MCP server, you can do from a terminal, a script or a CI job with the `kazari` CLI or the REST API. They share one service layer, so an artifact published one way can be updated any other way.

## Get a key

An administrator issues keys. A key carries scopes: `artifacts:read` to read, `artifacts:write` to publish and change, `artifacts:delete` to delete. For CI, ask for a key without the delete scope and with an expiry:

```sh
kazari admin keys issue ana --label ci --scopes artifacts:read,artifacts:write --expires 2160h
```

## Install the CLI

The `kazari` binary is the server and the client in one. Download it from a [release](https://github.com/PeterBooker/kazari/releases) for Linux (amd64, arm64) or macOS (arm64) and put it on your `PATH`, or build it from a checkout:

```sh
CGO_ENABLED=0 go build -o kazari ./cmd/kazari
install -m 0755 kazari ~/.local/bin/kazari
```

Point it at your instance with environment variables, a `.env` file in the working directory, or `~/.config/kazari/config.yaml`:

```sh
export KAZARI_URL=https://api.art.example.com
export KAZARI_TOKEN=kzk_...
kazari me          # who you are, your scopes and your storage use
```

## Publish a directory

```sh
kazari publish ./dist --slug q3-report --name "Q3 report" --meta team=finance
# https://q3-report.art.example.com/
```

The directory is streamed as a tar.gz bundle. Dot files are skipped (except `.well-known`), and so are symlinks.

Then, to change it:

| Command | Effect |
| ------- | ------ |
| `kazari update q3-report ./dist --label "fix chart"` | Adds a version. Files you send replace the same paths; files you leave out are **kept**. |
| `kazari publish ./dist --slug q3-report --update` | Adds a version that **mirrors** the directory: files missing from it are removed. Creates the artifact if it does not exist. |
| `kazari update q3-report --delete drafts/notes.html` | Removes one file in a new version. |
| `kazari update q3-report --name "Q3 report (final)"` | Changes fields only; no new version. |
| `kazari rollback q3-report 2` | Serves version 2 again. Nothing is deleted. |
| `kazari visibility q3-report unlisted` | Hides it from other people's listings. `unpublished` takes it offline. |
| `kazari visibility q3-report members` | Opens it only for people who sign in to the instance. |
| `kazari share create q3-report --expires 14d` | Prints a link that opens a members page for someone outside, for two weeks. `kazari share revoke` ends it. |
| `kazari stale q3-report --grace 72h` | Retires it after three days. `kazari unstale` cancels. |
| `kazari rm q3-report` | Soft-deletes it now; `--hard` purges it. |

Add `--json` to any command for the API's JSON response, which suits scripts:

```sh
url=$(kazari --json publish ./dist --slug q3-report --update | jq -r .url)
```

### Avoid overwriting someone else's work

Two people (or a person and an agent) can publish to the same artifact. To make sure you are building on the version you think you are, pass the latest version you saw; the call fails with `version_conflict` if someone else published in between:

```sh
kazari update q3-report ./dist --expected 4
```

`publish --update` performs this check for you.

### Single-page apps

An app that routes in the browser (React Router, Vue Router and the like) needs every unknown path without a file extension to serve `index.html`:

```sh
kazari publish ./dist --slug planner --meta kazari.spa=true
```

Use relative asset paths in your build (for example Vite's `base: "./"`) so the app also works on version addresses such as `planner--v3.art.example.com`.

## Preview deployments from GitHub Actions

Publish each pull request to its own address and let the preview retire itself after the pull request closes. Store the key as the repository secret `KAZARI_TOKEN`.

```yaml
name: Preview
on:
  pull_request:
    types: [opened, synchronize, reopened, closed]

jobs:
  preview:
    runs-on: ubuntu-latest
    env:
      KAZARI_URL: https://api.art.example.com
      KAZARI_TOKEN: ${{ secrets.KAZARI_TOKEN }}
      KAZARI_VERSION: 0.1.0
      SLUG: myapp-pr-${{ github.event.number }}
    steps:
      - name: Install the Kazari CLI
        run: |
          curl -fsSL "https://github.com/PeterBooker/kazari/releases/download/${KAZARI_VERSION}/kazari_${KAZARI_VERSION}_linux_amd64.tar.gz" \
            | tar -xz kazari
          sudo install -m 0755 kazari /usr/local/bin/kazari

      - uses: actions/checkout@v7
        if: github.event.action != 'closed'

      - name: Build
        if: github.event.action != 'closed'
        run: npm ci && npm run build

      - name: Publish
        if: github.event.action != 'closed'
        run: |
          url=$(kazari --json publish ./dist --slug "$SLUG" --update --label "${GITHUB_SHA::7}" | jq -r .url)
          echo "Preview: $url" >> "$GITHUB_STEP_SUMMARY"

      - name: Retire
        if: github.event.action == 'closed'
        run: kazari stale "$SLUG" --grace 72h
```

Each push becomes a new version, so reviewers can link to `myapp-pr-42--v3` and know it will not change under them. For the main branch, publish to a fixed slug the same way, without the retire step.

## The REST API

The CLI is a thin client over the REST API at `https://api.art.example.com/v1`, which any language can call with a bearer token. A page from inline JSON:

```sh
curl -sS https://api.art.example.com/v1/artifacts \
  -H "Authorization: Bearer $KAZARI_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "slug": "hello",
    "name": "Hello",
    "files": [{ "path": "index.html", "content": "<!doctype html><h1>Hello</h1>" }]
  }'
```

A whole site as a bundle:

```sh
tar -czf site.tar.gz -C dist .
curl -sS https://api.art.example.com/v1/artifacts \
  -H "Authorization: Bearer $KAZARI_TOKEN" \
  -F 'manifest={"slug":"q3-report","name":"Q3 report"};type=application/json' \
  -F 'bundle=@site.tar.gz;type=application/gzip'
```

Updates use `PUT /v1/artifacts/{slug}` with the same two formats and an `If-Match: "<version>"` header for the concurrency check. Bundles too large for one request go through chunked uploads. The [REST API reference](api.md) has every route, field and error code.

## Limits worth knowing

By default a version holds up to 50 MiB and 2,000 files, a single file up to 25 MiB, and each user has 2 GiB of storage, counted across every version of every artifact they own. The user API allows 60 writes a minute per key. Your instance may set other values: `kazari me` shows your quota and use, and errors name the limit that was hit.
