---
title: Publish from code
description: Publish to Kazari from a terminal, a script or CI with the kazari CLI and the REST API, including preview deployments that expire on their own.
---

# Publish from code

Everything an agent can do through the MCP server, you can do from a terminal, a script or a CI job with the `kazari` CLI or the REST API. They share one service layer, so an artifact published one way can be updated any other way.

This guide covers getting a key, installing the CLI, publishing and changing a directory, preview deployments from GitHub Actions, and calling the REST API directly.

## Getting a key

You create your own keys on your account page at `https://art.example.com/account/`, after signing in to the instance ([the account page](identity.md#the-account-page)). A key carries scopes, which decide what it can do:

- `artifacts:read` to read.
- `artifacts:write` to publish and change.
- `artifacts:share` to change who can open a page, and to create share links.
- `artifacts:delete` to delete.

For CI, create a key without the share and delete scopes and with an expiry, such as 90 days. The key is shown once, so store it straight away.

If your instance has no sign-in for you, an administrator issues the key instead:

```sh
kazari admin keys issue ana --label ci --scopes artifacts:read,artifacts:write --expires 90d
```

Once you hold a key, you can issue narrower ones yourself, for example one per CI job, restricted to the pages that job publishes:

```sh
kazari keys issue --label previews --scopes artifacts:read,artifacts:write --slugs 'pr-*' --expires 90d
```

That key sees and changes only artifacts whose slug starts with `pr-`. The [CLI reference](cli.md#keys) covers issuing, rotating and revoking your keys.

## Installing the CLI

The `kazari` binary is the server and the client in one. Download it from a [GitHub release](https://github.com/PeterBooker/kazari/releases) for Linux on amd64 or arm64, or macOS on arm64, and put it on your `PATH`.

To build it from a checkout instead, run:

```sh
CGO_ENABLED=0 go build -o kazari ./cmd/kazari
install -m 0755 kazari ~/.local/bin/kazari
```

Then point the CLI at your instance. `kazari login` asks for your key, checks it and saves both settings in `~/.config/kazari/config.yaml`:

```sh
kazari login --url https://api.art.example.com
kazari me          # who you are, your scopes and your storage use
```

In CI, set the environment variables `KAZARI_URL` and `KAZARI_TOKEN` instead. The settings can also live in a dotenv file named by `$KAZARI_ENV_FILE` ([CLI configuration](cli.md#configuring-the-client)).

## Publishing a directory

To publish, name the directory and choose a slug, which becomes the page's address:

```sh
kazari publish ./dist --slug q3-report --name "Q3 report" --meta team=finance
# prints q3-report v1 → https://q3-report.art.example.com/ (pinned: https://q3-report--v1.art.example.com/)
```

The CLI streams the directory as a tar.gz bundle and lists on stderr what it sent and skipped. It skips symlinks, every file or directory whose name starts with a dot, such as `.git`, except `.well-known`, and files that look like secrets, such as `*.pem` or a file holding an API key. A `.kazariignore` file leaves out more, and `--dry-run` shows the list without sending anything ([what is sent](cli.md#what-is-sent)).

To publish over the same slug on every build, use `kazari deploy` instead, which creates the artifact the first time ([redeploying a directory](#redeploying-a-directory)).

### Publishing a single-page app

An app that routes in the browser, with React Router, Vue Router or the like, needs every unknown path without a file extension to serve `index.html`. Set the metadata `kazari.spa` to `true` when you publish it:

```sh
kazari publish ./dist --slug planner --meta kazari.spa=true
```

Use relative asset paths in your build, such as Vite's `base: "./"`, so the app also works on version addresses like `planner--v3.art.example.com`.

## Redeploying a directory

To publish a build again, run `kazari deploy` with the same slug:

```sh
kazari deploy ./dist --slug q3-report --label "fix chart"
```

The new version holds exactly the files in `./dist`: files gone from the directory, such as a bundler's old `assets/index-<hash>.js`, are removed. If the slug does not exist yet, `deploy` creates it, so the same command serves the first build and every one after.

## Changing an artifact

These commands change an artifact after its first publish:

| Command | Effect |
| ------- | ------ |
| `kazari deploy ./dist --slug q3-report` | Adds a version that **mirrors** the directory: files missing from it are removed. Creates the artifact if it does not exist. |
| `kazari deploy ./dist --slug q3-report --stage` | Adds the mirrored version without publishing it. It opens at its version address for people who sign in, until `kazari rollback` publishes it. |
| `kazari update q3-report ./dist --label "fix chart"` | Adds a version. Files you send replace the same paths; files you leave out are **kept**. |
| `kazari update q3-report --delete drafts/notes.html` | Removes one file in a new version. |
| `kazari update q3-report --name "Q3 report (final)"` | Changes fields only; no new version. |
| `kazari rollback q3-report 2` | Serves version 2 again, or makes a staged version 2 live. Nothing is deleted. |
| `kazari visibility q3-report unlisted` | Hides it from other people's listings. `unpublished` takes it offline. |
| `kazari visibility q3-report members` | Opens it only for people who sign in to the instance. |
| `kazari share create q3-report --expires 14d` | Prints a link that opens a members page for someone outside, for two weeks. `kazari share revoke` ends it. |
| `kazari stale q3-report --grace 3d` | Retires it after three days. `kazari unstale` cancels. |
| `kazari rm q3-report` | Soft-deletes it now; `--hard` purges it. |

After `kazari rollback`, or while a version is staged, the next `deploy`, `update` or `edit` needs `--base` to say whether it builds on the version now served or on the latest one, or `--stage` to stage it too. Without either the command fails and prints the choices.

### Preventing overwrites

Two people, or a person and an agent, can publish to the same artifact. To make sure you build on the version you think you are, pass the latest version you saw with `--expected`:

```sh
kazari update q3-report ./dist --expected 4
```

If someone else published in between, the call fails with `version_conflict`. `kazari deploy` performs this check for you.

### Using JSON output in scripts

To use a result in a script, add `--json` to any command. It prints the API's JSON response instead of text:

```sh
url=$(kazari --json deploy ./dist --slug q3-report | jq -r .url)
```

Here `jq` takes the page's address from the response. A failure exits with a code that names its kind, such as `4` for a conflict or `3` for not found, and with `--json` prints the API's error object on stderr ([output and exit codes](cli.md#output-and-exit-codes)).

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
          url=$(kazari --json deploy ./dist --slug "$SLUG" --label "${GITHUB_SHA::7}" | jq -r .url)
          echo "Preview: $url" >> "$GITHUB_STEP_SUMMARY"

      - name: Retire
        if: github.event.action == 'closed'
        run: kazari stale "$SLUG" --grace 3d
```

On every push to a pull request, the workflow builds the site and publishes it to a slug made from the pull request's number, such as `myapp-pr-42`. When the pull request closes, it marks the preview stale with a 72-hour grace period. A key without the delete scope can do this only to artifacts its user owns, which is every preview it created; a slug that another member created first answers `403`.

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

To publish a whole site, send it as a bundle with a JSON manifest. A bundle is a tar.gz, a plain tar or a zip:

```sh
tar -czf site.tar.gz -C dist .
curl -sS https://api.art.example.com/v1/artifacts \
  -H "Authorization: Bearer $KAZARI_TOKEN" \
  -F 'manifest={"slug":"q3-report","name":"Q3 report"};type=application/json' \
  -F 'bundle=@site.tar.gz;type=application/gzip'
```

To update an artifact, send either format to `PUT /v1/artifacts/{slug}`, with an `If-Match: "<version>"` header for the concurrency check. `GET /v1/artifacts/{slug}` returns the latest version in its `ETag`. Bundles too large for one request go through [chunked uploads](api.md#chunked-uploads).

To deploy the way `kazari deploy` does, in one request, add `?create=true` and `"mirror": true`:

```sh
curl -sS -X PUT 'https://api.art.example.com/v1/artifacts/q3-report?create=true' \
  -H "Authorization: Bearer $KAZARI_TOKEN" \
  -F 'manifest={"mirror":true,"label":"nightly"};type=application/json' \
  -F 'bundle=@site.tar.gz;type=application/gzip'
```

`create=true` creates the artifact when the slug is free, and `mirror` removes the files the bundle does not hold ([update an artifact](api.md#update-an-artifact-put-artifactsslug)).

The [REST API reference](api.md) has every route, field and error code.

## Limits

By default, Kazari enforces these limits:

- **A version**: up to 50 MiB and 2,000 files.
- **A single file**: up to 25 MiB.
- **Storage**: 2 GiB per user, counted across every version of every artifact they own, with 4 KiB on top for each file of each version.
- **Writes**: 60 a minute per key on the user API.

Your instance can set other values. `kazari me` shows your quota and use, and errors name the limit that was hit.

## Next steps

- [CLI](cli.md): every `kazari` command and flag, output and exit codes.
- [REST API](api.md): every route, field and error code, including chunked uploads.
- [Publish with an agent](publish-with-an-agent.md): connecting an agent to the same instance, and sharing pages safely.
