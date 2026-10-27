---
title: Publish from code
description: Publish to Kazari from a terminal, a script or CI with the kazari CLI and the REST API, including preview deployments that expire on their own.
---

# Publish from code

Everything an agent can do through the MCP server, you can do from a terminal, a script or a CI job with the `kazari` CLI or the REST API. They share one service layer, so an artifact published one way can be updated any other way.

This guide covers getting a key, installing the CLI, publishing and changing a directory, preview deployments from GitHub Actions, and calling the REST API directly.

## Getting a key

An administrator issues keys. A key carries scopes, which decide what it can do:

- `artifacts:read` to read.
- `artifacts:write` to publish and change.
- `artifacts:delete` to delete.

For CI, ask for a key without the delete scope and with an expiry. This admin command issues one that expires after 90 days:

```sh
kazari admin keys issue ana --label ci --scopes artifacts:read,artifacts:write --expires 2160h
```

The key is printed once, so store it straight away.

## Installing the CLI

The `kazari` binary is the server and the client in one. Download it from a [GitHub release](https://github.com/PeterBooker/kazari/releases) for Linux on amd64 or arm64, or macOS on arm64, and put it on your `PATH`.

To build it from a checkout instead, run:

```sh
CGO_ENABLED=0 go build -o kazari ./cmd/kazari
install -m 0755 kazari ~/.local/bin/kazari
```

Then point the CLI at your instance. Export its address and your key, and check them with `kazari me`:

```sh
export KAZARI_URL=https://api.art.example.com
export KAZARI_TOKEN=kzk_...
kazari me          # who you are, your scopes and your storage use
```

The same settings can also live in a `.env` file in the working directory, or in `~/.config/kazari/config.yaml` ([CLI configuration](cli.md#configuring-the-client)).

## Publishing a directory

To publish, name the directory and choose a slug, which becomes the page's address:

```sh
kazari publish ./dist --slug q3-report --name "Q3 report" --meta team=finance
# prints https://q3-report.art.example.com/
```

The CLI streams the directory as a tar.gz bundle. It skips symlinks, and every file or directory whose name starts with a dot, such as `.git`, except `.well-known`.

### Publishing a single-page app

An app that routes in the browser, with React Router, Vue Router or the like, needs every unknown path without a file extension to serve `index.html`. Set the metadata `kazari.spa` to `true` when you publish it:

```sh
kazari publish ./dist --slug planner --meta kazari.spa=true
```

Use relative asset paths in your build, such as Vite's `base: "./"`, so the app also works on version addresses like `planner--v3.art.example.com`.

## Changing an artifact

These commands change an artifact after its first publish:

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

### Preventing overwrites

Two people, or a person and an agent, can publish to the same artifact. To make sure you build on the version you think you are, pass the latest version you saw with `--expected`:

```sh
kazari update q3-report ./dist --expected 4
```

If someone else published in between, the call fails with `version_conflict`. `publish --update` performs this check for you.

### Using JSON output in scripts

To use a result in a script, add `--json` to any command. It prints the API's JSON response instead of text:

```sh
url=$(kazari --json publish ./dist --slug q3-report --update | jq -r .url)
```

Here `jq` takes the page's address from the response.

## Deploying previews from GitHub Actions

Publish each pull request to its own address, and let the preview retire itself after the pull request closes. Store the key as the repository secret `KAZARI_TOKEN`, then add a workflow like this one, for example as `.github/workflows/preview.yml`:

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
        run: curl -fsSL https://kazari.io/install.sh | sh

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

On every push to a pull request, the workflow builds the site and publishes it to a slug made from the pull request's number, such as `myapp-pr-42`. When the pull request closes, it marks the preview stale with a 72-hour grace period.

Each push becomes a new version, so reviewers can link to `myapp-pr-42--v3` and know it will not change under them. For the main branch, publish to a fixed slug the same way, without the retire step.

## Using the REST API

The CLI is a thin client over the REST API at `https://api.art.example.com/v1`, which any language can call with a bearer token.

To publish a page from inline JSON, send its files in the request body:

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

To publish a whole site, send it as a tar.gz bundle with a JSON manifest:

```sh
tar -czf site.tar.gz -C dist .
curl -sS https://api.art.example.com/v1/artifacts \
  -H "Authorization: Bearer $KAZARI_TOKEN" \
  -F 'manifest={"slug":"q3-report","name":"Q3 report"};type=application/json' \
  -F 'bundle=@site.tar.gz;type=application/gzip'
```

To update an artifact, send either format to `PUT /v1/artifacts/{slug}`, with an `If-Match: "<version>"` header for the concurrency check. Bundles too large for one request go through [chunked uploads](api.md#chunked-uploads).

The [REST API reference](api.md) has every route, field and error code.

## Limits

By default, Kazari enforces these limits:

- **A version**: up to 50 MiB and 2,000 files.
- **A single file**: up to 25 MiB.
- **Storage**: 2 GiB per user, counted across every version of every artifact they own.
- **Writes**: 60 a minute per key on the user API.

Your instance can set other values. `kazari me` shows your quota and use, and errors name the limit that was hit.

## Next steps

- [CLI](cli.md): every `kazari` command and flag, output and exit codes.
- [REST API](api.md): every route, field and error code, including chunked uploads.
- [Publish with an agent](publish-with-an-agent.md): connecting an agent to the same instance, and sharing pages safely.
