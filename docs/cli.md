# CLI

The `kazari` binary is both the server and the client. `kazari serve` runs the server and `kazari check` tests its setup ([server commands](#server-commands)); every other command talks to a running instance over the [REST API](api.md).

This page covers installing and setting up the client, the server commands, then every client command with its flags. The artifact commands need the instance's API address and an API key (`kzk_...`); the admin commands need the admin socket on the server's host, or the admin address and a token.

## Usage

Every command has the same shape:

```text
kazari [flags] <command> [arguments] [flags]
```

Flags can come before or after the command and its arguments, so `kazari publish ./site --slug x` and `kazari --json publish --slug x ./site` are the same. `kazari --help` lists the commands, `kazari <command> --help` lists a command's flags, and `kazari help admin keys` does the same for any command.

## Installing the client

On a laptop, the install script puts the latest release into `/usr/local/bin`, after checking it against the release's checksums:

```sh
curl -fsSL https://kazari.io/install.sh | sh
```

`KAZARI_BINDIR=~/.local/bin` before `sh` installs it there instead, without `sudo`. With Go, `go install github.com/PeterBooker/kazari/cmd/kazari@latest` builds it from source. On a server the binary is already there: the package, the archive and the Docker image each carry it ([installing Kazari](install.md)).

## Configuring the client

The client needs two settings: the instance's API address and an API key. You create a key on your [account page](identity.md#the-account-page), `https://<base>/account/`.

### Signing in

To save both settings in the [config file](#the-config-file), run `kazari login` with the API address and paste the key when it asks:

```sh
kazari login --url https://api.art.example.com
```

The command checks the key against the instance before it saves anything, then prints who you are, such as `logged in to https://api.art.example.com as peter (member)`. It writes the file readable by you alone. Without a terminal, it reads the key from the first line of stdin, so `kazari login --url https://api.art.example.com < key.txt` works in a script.

`kazari logout` removes the key from the config file and leaves the URL. The key itself keeps working until you revoke it with [`kazari keys revoke`](#keys).

### Settings

Each setting comes from the first of these that is set: the flag, the environment variable, the config file. The settings are:

| Flag              | Variable              | Config key     | Meaning                                       |
| ----------------- | --------------------- | -------------- | --------------------------------------------- |
| `--url`           | `KAZARI_URL`          | `url`          | User API root, such as `https://api.art.example.com` |
| `--token`         | `KAZARI_TOKEN`        | `token`        | API key (`kzk_...`)                           |
| `--admin-url`     | `KAZARI_ADMIN_URL`    | `admin_url`    | Admin API root, such as `https://admin.art.example.com` |
| `--admin-token`   | `KAZARI_ADMIN_TOKEN`  | `admin_token`  | Admin token, or an admin user's key with the `admin` scope |
| `--admin-socket`  | `KAZARI_ADMIN_SOCKET` | `admin_socket` | Admin socket of a server on this host         |
| `--client-config` |                       |                | Config file path                              |
| `--json`          |                       |                | Print the JSON response instead of text, and API errors as JSON |
| `--timeout`       |                       |                | Give up on a request after this long, such as `5m` |
| `--allow-http`    | `KAZARI_ALLOW_HTTP=1` |                | Send the key to an `http://` URL on another machine |

The client takes a URL as people write it. A URL without a scheme means `https://`, or `http://` for `localhost`, a `*.localhost` name or a loopback address. A trailing `/` or `/v1` is dropped, since the client adds `/v1` itself.

By default, a request has no overall time limit, so a large upload can take as long as it needs, but the server must accept the connection within 10 seconds and start answering within 60. `--timeout` bounds the whole request.

The client sends a key over plain `http://` only to this machine. For another host it stops with an error that names `--allow-http`, which lets the request through.

When no admin URL is set and the user URL starts with `api.`, the client derives the admin URL as `admin.<base>`. When no admin token is set, the client tries the user token on the admin host.

### The admin socket

On the server's own host, the admin commands need no URL and no token: the server serves the admin API on a Unix socket in its data directory, and the `kazari` command finds it through the server's configuration, from `$KAZARI_CONFIG` or `/etc/kazari/kazari.yaml` with the `kazari.env` beside it, or `$KAZARI_DATA_DIR`, else in `./.data` under `serve --local`. The socket's permissions are the credential, so run the command as the service user or as root (`sudo kazari admin ...`), or inside the container (`docker compose exec kazari kazari admin ...`).

An explicit `--admin-socket` wins over everything, an admin URL with a token wins over a socket that was merely found, and a command that finds neither says so. `admin health` needs no token even over a URL.

### Environment variables and `.env`

The environment variables can also come from a dotenv file you name with `$KAZARI_ENV_FILE`, with one `KEY=VALUE` line each, as for the server:

```sh
# ~/.config/kazari/client.env
KAZARI_URL=https://api.art.example.com
KAZARI_TOKEN=kzk_...
```

```sh
export KAZARI_ENV_FILE=~/.config/kazari/client.env
```

A variable already set in the process environment wins over the file.

The client commands never read `./.env` on their own. The working directory can be a repository someone else wrote, and a URL from a file in it would receive the token in your config file. For the same reason, give `$KAZARI_ENV_FILE` an absolute path: a relative one is looked up in whichever directory you run `kazari` from.

### The config file

The config file is `$XDG_CONFIG_HOME/kazari/config.yaml`, else `~/.config/kazari/config.yaml`. [`kazari login`](#signing-in) writes it; to write it by hand, use these keys:

```yaml
url: https://api.art.example.com
token: kzk_abcdefgh_...
admin_url: https://admin.art.example.com   # optional
admin_token: ""                            # optional
```

The file is optional unless you name it with `--client-config`. An unknown key in it is an error that lists the known ones, such as `unknown key "api_key" in ~/.config/kazari/config.yaml (known: url, token, admin_url, admin_token, admin_socket)`.

A config file that holds `token` or `admin_token` must be readable by you alone. If other users can read it, the client stops and names the fix, `chmod 600 ~/.config/kazari/config.yaml`.

> [!WARNING]
> A dotenv file holding a token contains a credential too. Make it readable by you alone with `chmod 600`.

## Output and exit codes

By default, commands print short human-readable text on stdout: the result line after a publish, and aligned tables for lists. Notes, warnings and summaries go to stderr, so stdout stays the part to pipe.

For scripts, `--json` prints the API's JSON response instead, pretty-printed. It works as a global flag or after the command:

```sh
kazari --json get q3-report | jq -r '.artifact.published_version'
```

When the server refuses a command, stderr shows `kazari: <code>: <message>`, often followed by a line with the next step. Under `--json`, stderr carries the API's error object instead, on one line:

```json
{"error":{"code":"slug_taken","message":"slug \"q3-report\" is already taken"}}
```

A response that is not the API's, such as a web page or a proxy's error, is never printed. The message says which address answered and that the API usually is `https://api.<base>`.

### Exit codes

The exit code tells the kinds of failure apart, so a script can branch without matching text:

| Exit code | Meaning | API error codes |
| --------- | ------- | --------------- |
| `0` | Success | |
| `1` | Any other failure of the command or the server | |
| `2` | Usage or configuration error, such as a bad flag, a missing URL or token, a missing directory or a bad config file | |
| `3` | Not found | `not_found`, `version_not_found`, `file_not_found` |
| `4` | Conflict | `slug_taken`, `version_conflict`, `published_not_latest`, `anchor_mismatch` |
| `5` | Rate limited, after the retries | `rate_limited` |
| `6` | Not signed in, or not allowed | `unauthorized`, `forbidden` |

For example, a deploy job can tell a conflict from an outage:

```sh
kazari deploy ./dist --slug q3-report
case $? in
  0) echo deployed ;;
  4) echo "someone else published; try again" ;;
  *) exit 1 ;;
esac
```

### Retries

When the server answers `429` or `503`, the client waits as long as its `Retry-After` header says, at most 30 seconds, and tries again, up to two more times. It retries reads, and writes that name the version they expect, such as `update --expected`, which the server refuses rather than repeats. Uploads of a directory are not retried.

## Server commands

`serve` and `check` read the server's configuration rather than the client's, so the flags above do not apply to them, and their help leaves them out.

### serve

```text
kazari serve [--config PATH] [--env-file PATH]
kazari serve --local
```

Runs the server until it is interrupted. The configuration comes from `--config`, `$KAZARI_CONFIG` or `/etc/kazari/kazari.yaml`, else from the environment alone, and the secrets from `--env-file`, `$KAZARI_ENV_FILE`, `kazari.env` beside the configuration file, or a `.env` file in the working directory, which beside a configuration file only fills `${NAME}` references ([the `.env` file](configuration.md#the-env-file)). An invalid configuration exits with status 2 and lists every problem; any other failure, such as a port in use or a data directory another server holds, exits with status 1.

`--local` runs the preset for your own machine instead, with no DNS, certificate or token: `http://<slug>.kazari.localhost:8080/`, data in `./.data` ([the local preset](configuration.md#the-local-preset)).

To run the server on your own machine:

```sh
kazari serve --local
```

The log ends with a `serving` line that names the addresses and, on a fresh instance, a `no users yet` line with the next step. With built-in accounts, the default without an identity provider, that step is the setup link printed beside it on standard error ([creating the first admin](install.md#creating-the-first-admin)).

### check

```text
kazari check [--config PATH] [--env-file PATH]
kazari check --local
```

Loads the configuration as `serve` would, then checks what the first start depends on: that the data directory is writable, that the base domain, `api.` and a random subdomain resolve to this host, that the DNS provider token can write and remove a TXT record in the zone, that `tls.cert_file` and `tls.key_file` open and cover the base domain, that the listener ports are free, whether a server is listening on the admin socket, and that the identity provider answers discovery. Its first line names the configuration file and the dotenv files read, and an `override` line follows for each key a `KAZARI_*` variable set. It exits with status 2 for an invalid configuration, 1 if a check failed and 0 otherwise.

`check --local` checks the `--local` preset of `serve` in the same way.

With an OpenTelemetry endpoint set ([`telemetry.otlp`](configuration.md#telemetryotlp), or `OTEL_EXPORTER_OTLP_ENDPOINT`), it also sends one record of each signal that exports: a span and a log record named `kazari check`, and a gauge `kazari.check`. That tests the protocol, the path, TLS and the headers in one go:

```text
telemetry    ok    http/protobuf https://otlp.example.com/otlp: traces, metrics accepted
```

A refusal names what to fix: `401` and `403` point at `telemetry.otlp.headers`, a refused connection or a certificate error at the endpoint and `ca_file`. The test records stay in the backend like any other. `check` also says, as a `skip` line, when `/metrics` is not served on the public admin host because `telemetry.metrics.token` is empty.

To check a server's setup, run it on the server:

```sh
sudo kazari check
```

It prints one line per check; [checking the setup](install.md#checking-the-setup) shows the output and what each line means.

## Artifact commands

The commands in this section use the user URL and token, except `kazari version`, which needs neither.

### publish

```text
kazari publish <dir|file> [--slug <slug>] [--name N] [--description D] [--label L]
               [--visibility public|unlisted|members] [--meta key=value ...] [--dry-run]
```

Creates an artifact from a directory, or from one file. Without `--slug`, the slug is the directory's or the file's name, lower-cased, with other characters as hyphens: `./Q3 Report` becomes `q3-report`. Without `--visibility`, the artifact gets the instance's default visibility, `serve.default_visibility`.

To publish a directory as a new artifact:

```sh
kazari publish ./site --slug q3-report --name "Q3 report" --meta team=finance
```

The command prints what it sent on stderr, then the result on stdout:

```text
14 files, 312.0 KiB; skipped: .env (dotfile), logo.png (symlink)
q3-report v1 → https://q3-report.art.example.com/ (pinned: https://q3-report--v1.art.example.com/)
```

The second address always serves version 1, whichever version is published later. When the server finds something that stops the page rendering, such as no `index.html`, it adds a `warning:` line on stderr.

A slug that is already taken fails with `slug_taken`. To publish over an existing artifact, use [`kazari deploy`](#deploy).

#### Publishing one file

To publish a single page, name the file instead of a directory:

```sh
kazari publish ./q3-report.html
```

An HTML file is published as `index.html`, so it opens at the artifact's address. Any other file keeps its name.

#### What is sent

The directory is streamed as a tar.gz bundle of its regular files. A symlink as the directory itself is followed, so `./dist` can point at the build output. These are left out and listed as skipped:

- symlinks inside the directory;
- every file or directory whose name starts with a dot, except `.well-known`;
- files that look like secrets: `*.pem`, `*.key`, `*.p12`, `*.pfx`, `*.kdbx`, `id_rsa*`, `id_ed25519*`, `id_ecdsa*`, `credentials*`, and any file that contains a Kazari API key.

A missing directory is a usage error with exit code 2.

#### Leaving files out

To leave more files out, list them in a `.kazariignore` file at the top of the directory, in the style of `.gitignore`:

```text
# build leftovers
*.map
node_modules/
/drafts/
docs/**/draft-*
!.htaccess
```

A pattern without a `/` matches a name at any depth, a leading `/` or a `/` inside anchors it at the top, a trailing `/` matches directories only, and `**` matches any number of directories. A line starting with `!` sends what the lines above, or the rules in [what is sent](#what-is-sent), leave out, such as a dotfile the site needs. The last matching line wins.

#### Checking before you publish

To see what would be sent without sending anything, add `--dry-run`:

```sh
kazari publish ./site --slug q3-report --dry-run
```

It prints one line per file sent or skipped and needs no URL or key. With `--json`, it prints the list as JSON.

### deploy

```text
kazari deploy <dir|file> [--slug <slug>] [--base N] [--stage] [--name N] [--description D] [--label L]
              [--visibility public|unlisted|members] [--meta key=value ...] [--dry-run]
```

Makes an artifact match a directory, which is what a redeploy needs. If the slug does not exist, `deploy` creates it as `publish` would. If it does, `deploy` adds a version that holds exactly the directory's files: files that are gone from the directory are deleted from the new version, so a build's old hashed assets do not pile up.

To deploy a build, the first time and every time after:

```sh
kazari deploy ./dist --slug q3-report --label "numbers fixed"
```

It prints the same summary as `publish`, with the files it removed, such as `14 files, 312.0 KiB; removed: assets/index-3fa0c2.js`. When the files equal the latest version's, no version is added and stderr says `no changes: version 5 is already this content`.

The slug, what is sent, `.kazariignore` and `--dry-run` work as for [`publish`](#publish). With `--dry-run`, the lines also list the files the new version would delete.

`deploy` fails with `version_conflict` if someone else published between its read and its write. After a [rollback](#rollback), or while a version is staged, it needs `--base` or `--stage` to say which version to build on, and the error prints the commands to choose from.

#### Staging a deploy

To put a build where reviewers who sign in can see it, without changing the page everyone else sees, add `--stage`:

```sh
kazari deploy ./dist --slug q3-report --stage
# prints q3-report v6 staged → https://q3-report--v6.art.example.com/ (members only; https://q3-report.art.example.com/ serves v5 until kazari rollback q3-report 6)
```

The version opens at its pinned address only for people signed in to the instance, not for share links. A staged deploy builds on the latest version, so the next `--stage` carries on from it, and [`kazari rollback`](#rollback) with its number makes it live. `--stage` needs an existing artifact: a new one is always published as version 1. [Staging a version](api.md#staging-a-version) has the full rules.

`publish --update` is the same command under its old name.

### import

```text
kazari import <url> --slug <slug> [--name N] [--description D] [--label L]
               [--visibility public|unlisted|members] [--meta key=value ...] [--keep-scripts]
```

Copies a web page into a new artifact and prints its URL. The server fetches the page with its stylesheets, fonts and images, stores them under `assets/`, points links at the source site and removes the page's scripts; `--keep-scripts` keeps them. Without `--name` and `--description`, the page's title and meta description are used.

To copy a page as the starting point for a campaign site:

```sh
kazari import https://www.example.com/ --slug fundraiser
```

The command prints the artifact's address, then what the copy holds: how many files came from the source, which resources stayed remote and why, and any notes, such as a page that renders with JavaScript. With `--json` it prints the publish result, whose `import` field [the API reference](api.md#importing-a-web-page) describes.

A page behind a login or bot protection fails with `fetch_failed`. Save it from a browser instead and publish the files with `kazari publish`.

### update

```text
kazari update <slug> [dir] [--label L] [--expected N] [--base N] [--stage] [--keep-stale] [--dry-run]
              [--name N] [--description D] [--meta key=value ...] [--delete path ...]
```

Adds files from a directory as a new version, changes fields, or both.

Files in `dir` are added, or replace the files at the same paths. Files you do not send are **kept**: remove them with `--delete path`, or use [`kazari deploy`](#deploy) to make the artifact match the directory. Without a directory, `update` changes only the fields you name.

`--expected N` fails with `version_conflict` unless `N` is the latest version. After a [rollback](#rollback), `--base N` names the version to build on. `--stage` adds the version without publishing it, as [`deploy --stage`](#staging-a-deploy) does. A new version clears the stale mark unless you give `--keep-stale`. `--dry-run` lists what would be sent and deleted.

To add files, change fields or delete a file:

```sh
# add the files in ./site as a new version, if version 2 is still the latest
kazari update q3-report ./site --label "add appendix" --expected 2

# change fields only
kazari update q3-report --name "Q3 report (final)" --meta status=final

# delete one file
kazari update q3-report --delete drafts/notes.html
```

`--meta key=` sets an empty value. The CLI cannot delete a metadata key; send `null` through [`PATCH /artifacts/{slug}`](api.md#change-fields-patch-artifactsslug) instead.

### files ls

```text
kazari files ls <slug> [--version N]
```

Lists the files of the published version, or of version `N`, with their size, type and hash.

To see what version 1 held:

```sh
kazari files ls q3-report --version 1
```

### files cat

```text
kazari files cat <slug> <path> [--version N] [--anchors]
```

Writes one file of the published version, or of version `N`, to stdout. `--anchors` prefixes every line of a text file with its anchor (`12:3fa0|`), which `edit --start`, `--end`, `--after` and `--before` take.

To save version 1 of a page:

```sh
kazari files cat q3-report index.html --version 1 > old-index.html
```

### pull

```text
kazari pull <slug> [dir|-] [--version N] [--force]
```

Writes the files of the published version, or of version `N`, to a directory, to change them locally and [`deploy`](#deploy) the directory again. The directory defaults to `./<slug>`, so `kazari pull q3-report` and `kazari deploy q3-report` work on the same files. It must be empty or not exist yet.

To edit the published page locally and publish the result:

```sh
kazari pull q3-report
kazari deploy q3-report --label "fix chart"
```

It prints `pulled version 5 of q3-report to q3-report: 14 files, 312.0 KiB`. A staged version can be pulled by its number. The files of an unpublished or soft-deleted artifact can be pulled by the same people who can [read them](api.md#read-a-file-get-artifactsslugfilespath).

`--force` writes into a directory that is not empty: the files the version holds replace those at the same paths, and other files stay.

If the version holds files that `deploy` would leave out, such as dotfiles or names that look like secrets, stderr lists them. A mirrored deploy would delete them from the artifact, so list them with `!` in `.kazariignore` to keep sending them ([what is sent](#what-is-sent)).

To write the archive to standard output instead, as `tar.gz`, pass `-`:

```sh
kazari pull q3-report - > q3-report.tar.gz
```

### edit

```text
kazari edit <slug> <path> --old TEXT --new TEXT [--all] [--expected N] [--label L] [--stage] [--keep-stale]
kazari edit <slug> <path> --start ANCHOR [--end ANCHOR] --new TEXT [flags]
kazari edit <slug> <path> --after ANCHOR | --before ANCHOR --new TEXT [flags]
kazari edit <slug> <path> --edits FILE [flags]
```

Changes text in one file of the latest version and publishes the result as a new version. Every other file carries over. After a [rollback](#rollback), `--base N` names the version whose file to change, and `--stage` adds the new version without publishing it, as [`deploy --stage`](#staging-a-deploy) does. The same operation is [`POST /artifacts/{slug}/edit`](api.md#edit-a-file-post-artifactsslugedit) in the REST API and [`edit_file`](mcp.md#editing-files) over MCP.

`--expected N` fails unless the latest version is `N`. The command prints the new version and its address, then one line per edit with where it landed and how it matched.

#### Replacing text

`--old` must match the file once, or everywhere with `--all`. When it does not match exactly, whole lines are compared ignoring trailing whitespace, then ignoring indentation.

To change a heading:

```sh
kazari edit q3-report index.html --old "<h1>Q3 report</h1>" --new "<h1>Q4 report</h1>"
```

It prints:

```text
https://q3-report.art.example.com/
edit 1: lines 12-12 (exact)
```

#### Replacing lines by anchor

`--start` and `--end` replace whole lines, named by the anchors that `files cat --anchors` prints. `--new ""` deletes them.

To delete two paragraphs, first find their anchors:

```sh
kazari files cat q3-report index.html --anchors | sed -n 20,22p
```

It prints:

```text
20:7c1e|    <p>Revenue grew.</p>
21:e03a|    <p>Costs fell.</p>
22:41b9|  </main>
```

Then replace lines 20 to 21 with nothing:

```sh
kazari edit q3-report index.html --start 20:7c1e --end 21:e03a --new ""
```

It prints:

```text
https://q3-report.art.example.com/
edit 1: removed at line 20 (anchor)
```

#### Inserting lines

`--after` and `--before` insert `--new` as new lines next to the line with that anchor, which stays as it is. To add a paragraph after line 21:

```sh
kazari edit q3-report index.html --after 21:e03a --new "    <p>Margins held.</p>"
```

It prints:

```text
q3-report v4 → https://q3-report.art.example.com/ (pinned: https://q3-report--v4.art.example.com/)
edit 1: lines 22-22 (anchor)
```

#### Several edits at once

`--edits` reads a JSON array of edits from a file, or from stdin with `-`. Each edit is `{"old_text", "new_text", "replace_all"}`, `{"start", "end", "new_text"}`, or `{"after", "new_text"}` or `{"before", "new_text"}`.

### get

```text
kazari get <slug> [--files]
```

Shows the record and its versions, with the published one marked `*` and [staged ones](#staging-a-deploy) marked `staged`. `--files` adds file lists for the newest versions, up to 5,000 files, and says where it stopped; older ones are on the API's [next page](api.md#get-an-artifact-get-artifactsslug).

To see the newest versions' files:

```sh
kazari get q3-report --files
```

### list

```text
kazari list [--mine created|updated|any] [--meta key=value ...] [--search TEXT]
            [--visibility public|unlisted|members|unpublished] [--stale include|exclude|only]
            [--deleted include|exclude|only] [--limit N] [--cursor C] [--all]
```

Lists the artifacts you can see, newest-updated first. Flags you leave out take the server's defaults: no `--mine` filter, stale artifacts included, deleted ones excluded, and 50 per page.

`--search` keeps the artifacts with the text in their slug or name, ignoring the case of ASCII letters. `--visibility` keeps those with that visibility; `unpublished` means the ones taken offline.

When more pages exist, stderr prints the next `--cursor`. `--all` follows the cursors and prints everything.

To list the stale artifacts you created or updated, or every artifact with the metadata `team=finance`:

```sh
kazari list --mine any --stale only
kazari list --meta team=finance --all
```

### rollback

```text
kazari rollback <slug> <version>
```

Serves an existing version again. It moves a pointer rather than copying anything, so it can also move forward to a newer version, such as a [staged one](#staging-a-deploy).

To serve version 1 again:

```sh
kazari rollback q3-report 1
```

The command prints `q3-report now serves version 1:` and the address.

While an earlier version is published, `update`, `deploy` and `edit` need `--base` to say which version they build on: the published one, to change what is served, or the latest, to carry on from it. Without it they fail with `published_not_latest`, so the next change does not quietly undo the rollback, unless it is staged with `--stage`. Publishing the latest version again lifts this.

### versions

```text
kazari versions rm <slug> <version>...
kazari versions prune <slug> --keep N [--dry-run]
```

Deletes versions for good, to free the quota they take: every version counts in full, so a page edited often fills its owner's quota. Neither command deletes the published version or the latest one. Deleting versions of an artifact you do not own needs a key with `artifacts:delete`.

To keep the 20 newest versions and delete the rest, check first, then prune:

```sh
kazari versions prune q3-report --keep 20 --dry-run
# prints would delete versions 1, 2, 3 of q3-report
kazari versions prune q3-report --keep 20
```

A prune also keeps the published version and staged versions newer than it, however old. To delete particular versions instead, name them:

```sh
kazari versions rm q3-report 4 7
```

Each deleted version's address answers `410`, or `404` on a `members` page, and the next `gc` removes the files no other version holds. To prune a page on every sweep instead, set its metadata: `kazari update q3-report --meta kazari.keep_versions=20`. [Deleting versions](api.md#delete-a-version-delete-artifactsslugversionsn) in the API reference has the full rules.

### visibility

```text
kazari visibility <slug> public|unlisted|members|unpublished
```

Sets who can see the artifact. The values are:

- `public`: appears in everyone's listings and is served to anyone with the URL.
- `unlisted`: served to anyone with the URL, but left out of other people's listings.
- `members`: opens only for people who sign in to the instance in their browser, and for holders of a [share link](#share).
- `unpublished`: offline (`404`), with nothing deleted. Setting any of the other three brings the page back.

To make a page members-only:

```sh
kazari visibility q3-report members
```

The command prints `q3-report is now members`.

### share

```text
kazari share create <slug> [--label L] [--expires 7d] [--password]
kazari share list <slug>
kazari share revoke <slug> <prefix>
```

`create` prints a URL that opens a `members` artifact without signing in, for someone outside the instance. The link works until it expires or is revoked, and disabling the user who created it revokes it. `--expires` takes a duration such as `72h` or `14d`; by default a link lasts 7 days, and at most `serve.share_link_max_ttl`.

The URL holds the link's secret and is printed only this once. It goes to stdout while the prefix and expiry go to stderr, so `kazari share create q3-report --label "Acme" | pbcopy` copies only the URL.

To have the link ask for a password before it opens the page, add `--password`. On a terminal the command asks for it twice without showing it; otherwise it reads the first line of stdin, as in `kazari share create q3-report --password < password.txt`. The password is never an argument, so it stays out of the shell's history.

`list` shows each link's prefix, label, creator, expiry, uses, whether it has a password and its state (`active`, `expired` or `revoked`), never its URL. `revoke` ends a link at once.

To make a page members-only, share it with a reviewer for two weeks, then end the link:

```sh
kazari visibility q3-report members
kazari share create q3-report --label "Acme review" --expires 14d
kazari share revoke q3-report ab2cd3ef
```

### stale, unstale

```text
kazari stale <slug> [--grace 336h]
kazari unstale <slug>
```

`stale` starts the grace period after which the artifact is soft-deleted, and the page keeps serving meanwhile. `--grace` is a duration such as `72h`, `3d` or `2w`; without it, the server's `lifecycle.default_grace` applies. `unstale` cancels the grace period. A key without `artifacts:delete` can mark only its own user's artifacts stale.

To retire a page in three days:

```sh
kazari stale q3-report --grace 3d
```

The command prints when the artifact became stale and when it expires.

### rm

```text
kazari rm <slug> [--hard [--yes]]
```

Soft-deletes an artifact, which stays restorable until the retention period ends. `--hard` purges it now. Both need a key with the `artifacts:delete` scope.

On a terminal, `--hard` asks you to type the slug before it purges. Without a terminal, as in a script, it needs `--yes` instead.

To delete a page:

```sh
kazari rm q3-report
```

The command prints `deleted q3-report`, or `purged q3-report` with `--hard`.

> [!WARNING]
> `--hard` cannot be undone: it purges the artifact at once, with no retention period.

A deleted artifact keeps its slug. Publishing to the slug fails with `slug_taken`, and the message says to restore it with `kazari restore`. A purged artifact keeps its slug for a while longer, and the message names the day it is free again.

### restore

```text
kazari restore <slug>
```

Restores a soft-deleted artifact, which serves again at its address. A purged artifact cannot be restored, and the command fails with the server's error.

To bring a deleted page back:

```sh
kazari restore q3-report
```

The command prints `restored q3-report:` and the page's address. `kazari list --deleted only` shows what can still be restored.

### skill

```text
kazari skill
```

Prints the instance's Agent Skill: a `SKILL.md` generated from its configuration, with the MCP workflow, authoring conventions, limits and error recipes.

To install it for Claude Code:

```sh
mkdir -p ~/.claude/skills/kazari && kazari skill > ~/.claude/skills/kazari/SKILL.md
```

### keys

```text
kazari keys [list]
kazari keys issue [--label L] [--scopes a,b] [--slugs a,b] [--expires 90d]
kazari keys rotate <prefix> [--overlap 24h]
kazari keys revoke <prefix>
```

Lists, issues, rotates and revokes your own API keys. `kazari keys` alone lists them: prefix, label, scopes, slug patterns, the key each was issued with, created, last used, expiry and revocation.

The key in `--token` bounds the other three commands. It issues, rotates or revokes only a key that holds no scope it lacks and reaches no artifact it does not. [Your own keys](api.md#your-own-keys-mekeys) has the rules in full.

To issue a key for an agent that keeps the weekly reports current, for 90 days:

```sh
kazari keys issue --label weekly-report --scopes artifacts:read,artifacts:write \
  --slugs 'weekly-*' --expires 90d
```

The key is printed once on stdout, and a line describing it on stderr. Without `--scopes`, `--slugs` or `--expires`, the new key gets the scopes, restriction and expiry of the key that issues it.

- `--slugs`: a comma-separated list of [slug patterns](api.md#restricted-credentials) the key is restricted to: an exact slug, or a prefix ending in `*`. Quote the value, so the shell leaves `*` alone.
- `--expires`: a lifetime such as `720h` or `90d`.

To replace a key, rotate it:

```sh
kazari keys rotate abcdefgh --overlap 24h
```

`rotate` prints a new key with the same label, scopes, restriction and lifetime. The old key keeps working for `--overlap`, which is 24 hours by default and at most `7d`; `--overlap 0` ends it at once. The new key expires no later than the key in `--token`, so a key that rotates itself gets a new secret and no more time.

`revoke` ends a key at once, with every key that was issued with it and every OAuth connection approved with one of them.

A person holds 50 live keys at most. At that number, `issue` fails with `too_many_keys` until a key is revoked or expires. `rotate` still works, because the key it replaces does not count against the limit.

The [account page](identity.md#the-account-page) does the same from a browser, with no key in hand.

### me, version

These two commands print information about you and this binary:

```text
kazari me        # handle, role, scopes, credential source, slug patterns, expiry, storage used of quota
kazari version   # version, commit, the commit's date and Go version of this binary
```

An admin whose key lacks the `admin` scope sees `role: admin (this key has no admin scope)`: the key acts as a member's.

## Admin commands

Admin commands manage users, invites, keys, identities, OAuth grants, artifacts and the server, and read the audit log and page views. On the server's host they use [the admin socket](#the-admin-socket) and need no token; elsewhere they use the admin URL and token from [configuring the client](#configuring-the-client). `admin health` needs no token either way.

The [dashboard](dashboard.md) does the same from a browser: it has no task that these commands lack.

| Command                                                                 | Does                                                        |
| ----------------------------------------------------------------------- | ----------------------------------------------------------- |
| `admin users list`                                                      | List users with usage, quota, state (`active`, `invited` or `disabled`) and last activity |
| `admin users show <handle>`                                             | One user: whether a password is set, passkeys, unused recovery codes, live keys, browser sessions and last activity |
| `admin users create <handle> [--email E] [--name N] [--role member\|admin] [--quota 500MiB]` | Create a user                        |
| `admin users invite <handle> [--email E] [--name N] [--role R] [--quota Q] [--replace]` | Create a user and print the link that lets them set a password; `--replace` prints a new link for an existing user |
| `admin users reset-password <handle> [--revoke-all]`                    | Print a link that lets a user set a new password            |
| `admin users sign-out <handle>`                                         | End every browser session of a user; keys and connected apps stay |
| `admin users update <handle> [--disable\|--enable] [--role R] [--quota Q] [--email E] [--name N]` | Change a user; `--disable` also revokes every key, OAuth grant and share link they created, and their unused invite and reset links |
| `admin users transfer <handle> <owner>`                                 | Give every artifact a user owns to another user             |
| `admin invites list`                                                    | List the invite, reset and setup links that still work      |
| `admin invites revoke <id>`                                             | End a link before it is used                                |
| `admin setup-link`                                                      | Print a link that creates the first admin of a fresh instance |
| `admin keys list <handle>`                                              | List a user's keys                                          |
| `admin keys issue <handle> [--label L] [--scopes a,b] [--slugs a,b] [--expires 90d]` | Issue a key; prints the key once on stdout     |
| `admin keys rotate <handle> <prefix> [--overlap 24h]`                   | Replace a key with a new one; prints the new key once       |
| `admin keys revoke <handle> <prefix>`                                   | Revoke a key and every key issued with it                   |
| `admin identities list <handle>`                                        | List the identities a user signs in with at the identity provider |
| `admin identities link <handle> --issuer URL --subject SUB [--email E]` | Link an identity to a user, moving it from any other user   |
| `admin identities unlink <handle> --issuer URL --subject SUB`           | Remove one of a user's identities                           |
| `admin grants list <handle>`                                            | List a user's OAuth grants                                  |
| `admin grants revoke <id>`                                              | Revoke an OAuth grant                                       |
| `admin artifacts list [list flags] [--owner H]`                         | Every artifact with its owner; same flags as `list`, plus `--owner` for one user's |
| `admin transfer <slug> <handle>`                                        | Give an artifact to another user                            |
| `admin hide <slug>`                                                     | Unpublish an artifact (abuse response)                      |
| `admin settings`                                                        | Effective server configuration, secrets redacted, with where each value that is not a default was set |
| `admin jobs list`                                                       | Scheduler jobs: interval, last and next run, last error     |
| `admin jobs run <name>`                                                 | Run a job now and wait for it                               |
| `admin audit [--since T] [--until T] [--slug S] [--actor H] [--subject H] [--action A ...] [--limit N] [--cursor C]` | Audit log, newest first                |
| `admin views [<slug>] [--days N]`                                       | Page views of the instance, or of one artifact with its files and the members who opened it; `--days` defaults to 30 |
| `admin backup <dir> [--no-certs]`                                       | Write a backup of the database and every blob into a directory, or as tar to standard output with `-` |
| `admin restore-backup <dir> [--data-dir PATH] [--force]`                | Restore a backup directory into the data directory of a stopped server on this host |
| `admin ready [--fresh]`                                                 | Check the running server is set up correctly; exits 1 when a check fails. Also says when a newer release is known |
| `admin health`                                                          | Server status and version                                   |

### Flag values

The admin flags take these values:

- `--quota`: bytes or a unit, such as `500MiB` or `2GB`; `0` means unlimited. A new user's quota defaults to `limits.default_user_quota`.
- `--name`: the person's display name, such as `"Ana Lima"`. On `users update`, `--name ""` clears it.
- `--scopes`: a comma-separated list from `artifacts:read`, `artifacts:write`, `artifacts:share`, `artifacts:delete` and `admin`, which only admin users can hold. By default, a key gets the four artifact scopes.
- `--slugs`: a comma-separated list of [slug patterns](api.md#restricted-credentials) that restricts the key to some artifacts, such as `'weekly-*'`. By default, a key is not restricted.
- `--expires`: a lifetime such as `720h` or `90d`. By default, a key never expires, or gets `auth.tokens.max_key_ttl` when that is set.
- `--overlap`: how long a rotated key keeps working, such as `24h` (the default), `7d` (the most) or `0`.
- `--since` and `--until`: an RFC 3339 time, or a duration meaning "that long ago", such as `24h`, `3d` or `2w`. `--until` leaves out events at or after its time.
- `--action`: an exact action such as `key.issue`, or a prefix ending in a dot such as `artifact.`. Repeat it to match several.

For example, to create a user with a 1 GiB quota, issue a CI key valid for 90 days, run the sweep job, then read the last day's audit entries for one artifact:

```sh
kazari admin users create ana --email ana@example.com --quota 1GiB
kazari admin keys issue ana --label ci --scopes artifacts:read,artifacts:write --expires 90d
kazari admin jobs run sweep
kazari admin audit --since 24h --slug q3-report
```

### Page views

To see how often pages were opened in the last 30 days, run `admin views`. It prints the totals, the most viewed artifacts and each day with views:

```sh
kazari admin views
```

To see one artifact over the last week, with its most viewed files and the members who opened it, name it:

```sh
kazari admin views q3-report --days 7
```

`--days` runs from 1 to the days of `analytics.retention`, today included. With analytics off, both forms fail with `analytics is off on this instance` ([page views](ops.md#page-views)).

### OAuth grants

An OAuth grant is one MCP client's connection for one user. List a user's grants, then revoke one by its id:

```sh
kazari admin grants list ana
kazari admin grants revoke 17
```

`list` prints, for each grant, its id, the client, the scopes, the API key it was approved with if any, the last refresh and the expiry. The id is the number in the `oauth:<id>` credential of the [audit log](ops.md#audit-log), and stays the same across refreshes. `revoke` ends the connection at once; the client must sign in again.

### Invites and reset links

On an instance with [built-in accounts](identity.md#built-in-accounts), a person sets their password through a link. These commands print one:

```sh
kazari admin users invite ana --email ana@example.com --name "Ana Lima"
kazari admin users invite ana --replace
kazari admin users reset-password ana
kazari admin setup-link
```

Each prints the link alone on stdout, and on stderr a line saying it works once and until when. The link is never shown again: a lost one is replaced with the same command, or `--replace` for an invite, which ends the link before it.

- `invite` creates the user and their invite link. The flags are those of `users create`.
- `invite --replace` is for a user who has not set a password yet. For a user who has one it fails with `invalid_argument`, and `reset-password` is the command to use.
- `reset-password` is for a user who exists. With `--revoke-all`, using the link also revokes the user's API keys, removes their passkeys and recovery codes, and disconnects their apps.
- `setup-link` works only while the instance has no user, and fails with `setup_done` afterwards.

To see the links that still work, or end one before it is used:

```sh
kazari admin invites list
kazari admin invites revoke 12
```

`list` prints each link's id, user, purpose, who made it, when, and its expiry, never the link itself. A link leaves the list when it is used, replaced, revoked or expired, when its user sets or changes their password another way, and when its user is disabled. On an instance with built-in accounts off, the commands that make a link fail with `forbidden`.

### Identities

On an instance with an identity provider, an identity is one account at the provider, named by its issuer and subject, that signs in as a user. Kazari links one at a person's first sign-in; these commands link one ahead of that, or pin a user to an account whatever its email ([admin-created users](identity.md#admin-created-users)):

```sh
kazari admin identities list jdoe
kazari admin identities link jdoe --issuer https://accounts.google.com --subject 1234567890
kazari admin identities unlink jdoe --issuer https://accounts.google.com --subject 1234567890
```

`list` prints each identity's issuer, subject, email and last sign-in, which is `-` for an identity that was linked here and has not signed in yet. `link` moves an identity that another user holds to this one. `unlink` fails with `not_found` for an identity the user does not have. `--issuer` is the provider's issuer URL as in `auth.oidc.issuer`, and `--subject` the person's `sub` claim there.

### Backups

`admin backup` writes the database and every blob into a directory that is empty or does not exist yet:

```sh
sudo kazari admin backup /backup/kazari/2026-11-21
```

It prints the number of files, their size and the directory. Over the admin socket the backup includes `certmagic/`, the certificate and its private key, unless `--no-certs` is given; over an admin URL it never does, and the command says so. [Backing up and restoring](ops.md#backing-up-and-restoring) covers scheduling and restoring.

The command checks the directory before it asks the server. A directory that holds anything, or a path that is a file, ends it with exit status 2 and no backup is started.

To send the archive elsewhere, such as out of a container, name `-` for standard output:

```sh
docker compose exec -T kazari kazari admin backup - > kazari-2026-11-21.tar
```

`admin restore-backup` puts a backup directory back. It runs on the server's host with the server stopped, and needs no server: it reads the data directory from the server's configuration, or from `--data-dir`:

```sh
sudo kazari admin restore-backup /backup/kazari/2026-11-21 --force
```

It refuses, with exit status 2, while a server holds the data directory, and refuses a data directory that already holds an instance unless `--force` is given. The restored files belong to the data directory's owner. [Restoring from a backup](ops.md#restoring-from-a-backup) has the steps for each way of running Kazari.

With `--json` it prints `dir`, `files`, `bytes` and `certificates`, which is `true` when the directory holds `certmagic/`.

### Readiness

`admin ready` asks the running server to check its configuration, DNS records, DNS token, certificate, identity provider and disk:

```sh
sudo kazari admin ready
```

It prints one line per check and exits with status 1 when one fails. The server runs the checks once when it starts and answers from its last run when that is under 15 minutes old; `--fresh` runs the checks now, which can take a minute. [Checking readiness](ops.md#checking-readiness) lists the checks.

When the server's [update check](ops.md#the-update-check) knows of a newer release, the command prints one more line that names it. With `--json`, the answer is under `update`.
