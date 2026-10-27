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

Each setting comes from the first of these that is set: the flag, the environment variable, the config file. The settings are:

| Flag              | Variable              | Config key     | Meaning                                       |
| ----------------- | --------------------- | -------------- | --------------------------------------------- |
| `--url`           | `KAZARI_URL`          | `url`          | User API root, such as `https://api.art.example.com` |
| `--token`         | `KAZARI_TOKEN`        | `token`        | API key (`kzk_...`)                           |
| `--admin-url`     | `KAZARI_ADMIN_URL`    | `admin_url`    | Admin API root, such as `https://admin.art.example.com` |
| `--admin-token`   | `KAZARI_ADMIN_TOKEN`  | `admin_token`  | Admin token, or an admin user's key with the `admin` scope |
| `--admin-socket`  | `KAZARI_ADMIN_SOCKET` | `admin_socket` | Admin socket of a server on this host         |
| `--client-config` |                       |                | Config file path                              |
| `--json`          |                       |                | Print the JSON response instead of text       |

Give the URLs without `/v1`, which the client adds. When no admin URL is set and the user URL starts with `api.`, the client derives the admin URL as `admin.<base>`. When no admin token is set, the client tries the user token on the admin host.

### The admin socket

On the server's own host, the admin commands need no URL and no token: the server serves the admin API on a Unix socket in its data directory, and the `kazari` command finds it through the server's configuration, from `$KAZARI_CONFIG`, `/etc/kazari/kazari.yaml` or `$KAZARI_DATA_DIR`, else in `./.data` under `serve --local`. The socket's permissions are the credential, so run the command as the service user or as root (`sudo kazari admin ...`), or inside the container (`docker compose exec kazari kazari admin ...`).

An explicit `--admin-socket` wins over everything, an admin URL with a token wins over a socket that was merely found, and a command that finds neither says so. `admin health` needs no token even over a URL.

### Environment variables and `.env`

The environment variables can also come from a `.env` file in the working directory, with one `KEY=VALUE` line each, as for the server:

```sh
KAZARI_URL=https://api.art.example.com
KAZARI_TOKEN=kzk_...
```

A variable already set in the process environment wins over the file.

### The config file

The config file is `$XDG_CONFIG_HOME/kazari/config.yaml`, else `~/.config/kazari/config.yaml`. It is optional unless you name it with `--client-config`, and an unknown key in it is an error:

```yaml
url: https://api.art.example.com
token: kzk_abcdefgh_...
admin_url: https://admin.art.example.com   # optional
admin_token: ""                            # optional
```

> [!WARNING]
> The config file, and any `.env` holding a token, contain credentials. Make them readable by you alone with `chmod 600`.

## Output and exit codes

By default, commands print short human-readable text: a URL after a publish, and aligned tables for lists.

For scripts, `--json` prints the API's JSON response instead, pretty-printed. It works as a global flag or after the command:

```sh
kazari --json get q3-report | jq -r '.artifact.published_version'
```

Every command exits with one of three codes:

| Exit code | Meaning                                                                       |
| --------- | ----------------------------------------------------------------------------- |
| `0`       | Success                                                                       |
| `1`       | The command or the server failed; stderr shows `kazari: <code>: <message>`    |
| `2`       | Usage or configuration error, such as a bad flag, a missing URL or token, or a bad config file |

## Server commands

`serve` and `check` read the server's configuration rather than the client's, so the flags above do not apply to them.

### serve

```text
kazari serve [--config PATH] [--env-file PATH]
kazari serve --local
```

Runs the server until it is interrupted. The configuration comes from `--config`, `$KAZARI_CONFIG` or `/etc/kazari/kazari.yaml`, else from the environment alone, and the secrets from `--env-file`, `$KAZARI_ENV_FILE` or a dotenv file in the working directory ([how configuration is loaded](configuration.md#how-configuration-is-loaded)). An invalid configuration exits with status 2 and lists every problem; any other failure, such as a port in use, exits with status 1.

`--local` runs the preset for your own machine instead, with no DNS, certificate or token: `http://<slug>.localhost:8080/`, data in `./.data` ([the local preset](configuration.md#the-local-preset)).

To run the server on your own machine:

```sh
kazari serve --local
```

The log ends with a `serving` line that names the addresses and, on a fresh instance, a `no users yet` line with the next command.

### check

```text
kazari check [--config PATH] [--env-file PATH]
kazari check --local
```

Loads the configuration as `serve` would, then checks what the first start depends on: that the data directory is writable, that the base domain, `api.` and a random subdomain resolve to this host, that the DNS provider token can write and remove a TXT record in the zone, that the listener ports are free, and whether a server is listening on the admin socket. It exits with status 2 for an invalid configuration, 1 if a check failed and 0 otherwise.

To check a server's setup, run it on the server:

```sh
sudo kazari check
```

It prints one line per check; [checking the setup](install.md#checking-the-setup) shows the output and what each line means.

## Artifact commands

The commands in this section use the user URL and token, except `kazari version`, which needs neither.

### publish

```text
kazari publish <dir> --slug <slug> [--name N] [--description D] [--label L]
               [--visibility public|unlisted|members] [--meta key=value ...] [--update]
```

Creates an artifact from a directory and prints its URL. Without `--visibility`, the artifact gets the instance's default visibility, `serve.default_visibility`.

The directory is streamed as a tar.gz bundle of regular files only. Symlinks are skipped, and so is every file or directory whose name starts with a dot, except `.well-known`.

To publish a directory as a new artifact:

```sh
kazari publish ./site --slug q3-report --name "Q3 report" --meta team=finance
```

The command prints the artifact's address, `https://q3-report.art.example.com/`.

#### Mirroring a directory

With `--update`, an existing slug gets a new version that **mirrors** the directory: files missing from the directory are deleted. The call fails with `version_conflict` if someone else published in between. A slug that does not exist yet is created as usual.

To publish the directory again as the next version:

```sh
kazari publish ./site --slug q3-report --update --label "numbers fixed"
```

### update

```text
kazari update <slug> [dir] [--label L] [--expected N] [--keep-stale]
              [--name N] [--description D] [--meta key=value ...] [--delete path ...]
```

Adds a version from a directory, changes fields, or both.

Files in `dir` are added, or replace the files at the same paths. Files you do not send are **kept**: remove them with `--delete path`, or use `publish --update` to mirror the directory. Without a directory, `update` changes only the fields you name.

`--expected N` fails with `version_conflict` unless `N` is the latest version. A new version clears the stale mark unless you give `--keep-stale`.

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

### files cat

```text
kazari files cat <slug> <path> [--version N] [--anchors]
```

Writes one file of the published version, or of version `N`, to stdout. `--anchors` prefixes every line of a text file with its anchor (`12:3fa0|`), which `edit --start` and `--end` take.

To save version 1 of a page:

```sh
kazari files cat q3-report index.html --version 1 > old-index.html
```

### edit

```text
kazari edit <slug> <path> --old TEXT --new TEXT [--all] [--expected N] [--label L] [--keep-stale]
kazari edit <slug> <path> --start ANCHOR [--end ANCHOR] --new TEXT [flags]
kazari edit <slug> <path> --edits FILE [flags]
```

Changes text in one file of the latest version and publishes the result as a new version. Every other file carries over. The same operation is [`POST /artifacts/{slug}/edit`](api.md#edit-a-file-post-artifactsslugedit) in the REST API and [`edit_file`](mcp.md#editing-files) over MCP.

`--expected N` fails unless the latest version is `N`. The command prints the URL, then one line per edit with where it landed and how it matched.

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

#### Several edits at once

`--edits` reads a JSON array of edits from a file, or from stdin with `-`. Each edit is either `{"old_text", "new_text", "replace_all"}` or `{"start", "end", "new_text"}`.

### get

```text
kazari get <slug> [--files]
```

Shows the record and its versions, with the published one marked `*`. `--files` adds each version's file list.

To see every version's files:

```sh
kazari get q3-report --files
```

### list

```text
kazari list [--mine created|updated|any] [--meta key=value ...] [--stale include|exclude|only]
            [--deleted include|exclude|only] [--limit N] [--cursor C] [--all]
```

Lists the artifacts you can see, newest-updated first. Flags you leave out take the server's defaults: no `--mine` filter, stale artifacts included, deleted ones excluded, and 50 per page.

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

Serves an existing version again. It moves a pointer rather than copying anything, so it can also move forward to a newer version.

To serve version 1 again:

```sh
kazari rollback q3-report 1
```

The command prints `q3-report now serves version 1:` and the address.

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
kazari share create <slug> [--label L] [--expires 7d]
kazari share list <slug>
kazari share revoke <slug> <prefix>
```

`create` prints a URL that opens a `members` artifact without signing in, for someone outside the instance. The link works until it expires or is revoked. `--expires` takes a duration such as `72h` or `14d`; by default a link lasts 7 days, and at most `serve.share_link_max_ttl`.

The URL holds the link's secret and is printed only this once. It goes to stdout while the prefix and expiry go to stderr, so `kazari share create q3-report --label "Acme" | pbcopy` copies only the URL.

`list` shows each link's prefix, label, creator, expiry, uses and state (`active`, `expired` or `revoked`), never its URL. `revoke` ends a link at once.

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

`stale` starts the grace period after which the artifact is soft-deleted, and the page keeps serving meanwhile. `--grace` is a Go duration in hours, such as `72h` or `336h`; without it, the server's `lifecycle.default_grace` applies. `unstale` cancels the grace period.

To retire a page in three days:

```sh
kazari stale q3-report --grace 72h
```

The command prints when the artifact became stale and when it expires.

### rm

```text
kazari rm <slug> [--hard]
```

Soft-deletes an artifact, which stays restorable until the retention period ends. `--hard` purges it now. Both need a key with the `artifacts:delete` scope.

To delete a page:

```sh
kazari rm q3-report
```

The command prints `deleted q3-report`, or `purged q3-report` with `--hard`.

> [!WARNING]
> `--hard` cannot be undone: it purges the artifact at once, with no retention period.

### skill

```text
kazari skill
```

Prints the instance's Agent Skill: a `SKILL.md` generated from its configuration, with the MCP workflow, authoring conventions, limits and error recipes.

To install it for Claude Code:

```sh
mkdir -p ~/.claude/skills/kazari && kazari skill > ~/.claude/skills/kazari/SKILL.md
```

### me, keys, version

These three commands print information about you and this binary:

```text
kazari me        # handle, admin flag, scopes, credential source, storage used of quota
kazari keys      # your API keys: prefix, label, scopes, created, last used, expiry, revocation
kazari version   # version, commit, build date and Go version of this binary
```

## Admin commands

Admin commands manage users, keys, artifacts and the server. On the server's host they use [the admin socket](#the-admin-socket) and need no token; elsewhere they use the admin URL and token from [configuring the client](#configuring-the-client). `admin health` needs no token either way.

| Command                                                                 | Does                                                        |
| ----------------------------------------------------------------------- | ----------------------------------------------------------- |
| `admin users list`                                                      | List users                                                  |
| `admin users create <handle> [--email E] [--role member\|admin] [--quota 500MiB]` | Create a user                                    |
| `admin users update <handle> [--disable\|--enable] [--role R] [--quota Q] [--email E]` | Change a user; `--disable` also revokes every key |
| `admin keys list <handle>`                                              | List a user's keys                                          |
| `admin keys issue <handle> [--label L] [--scopes a,b] [--expires 720h]` | Issue a key; prints the key once on stdout                  |
| `admin keys revoke <handle> <prefix>`                                   | Revoke a key                                                |
| `admin artifacts list [list flags]`                                     | Every artifact with its owner; same flags as `list`         |
| `admin transfer <slug> <handle>`                                        | Give an artifact to another user                            |
| `admin hide <slug>`                                                     | Unpublish an artifact (abuse response)                      |
| `admin settings`                                                        | Effective server configuration, secrets redacted            |
| `admin jobs list`                                                       | Scheduler jobs: interval, last and next run, last error     |
| `admin jobs run <name>`                                                 | Run a job now and wait for it                               |
| `admin audit [--since T] [--slug S] [--limit N]`                        | Audit log, newest first                                     |
| `admin health`                                                          | Server status and version                                   |

### Flag values

The admin flags take these values:

- `--quota`: bytes or a unit, such as `500MiB` or `2GB`; `0` means unlimited. A new user's quota defaults to `limits.default_user_quota`.
- `--scopes`: a comma-separated list from `artifacts:read`, `artifacts:write`, `artifacts:delete` and `admin`, which only admin users can hold. By default, a key gets the three artifact scopes.
- `--since`: an RFC 3339 time, or a Go duration meaning "that long ago", such as `24h`.

For example, to create a user with a 1 GiB quota, issue a CI key valid for 90 days, run the sweep job, then read the last day's audit entries for one artifact:

```sh
kazari admin users create ana --email ana@example.com --quota 1GiB
kazari admin keys issue ana --label ci --scopes artifacts:read,artifacts:write --expires 2160h
kazari admin jobs run sweep
kazari admin audit --since 24h --slug q3-report
```

### Identities and OAuth grants

The CLI has no commands for linking identities, or for listing and revoking OAuth grants. Use the [admin API](api.md#admin-api) for those.
