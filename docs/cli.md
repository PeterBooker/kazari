# CLI

The `kazari` binary is both the server and the client. `kazari serve` runs the server ([install.md](install.md)); every other command talks to a running instance over the [REST API](api.md).

```text
kazari [global flags] <command> [arguments] [flags]
```

Flags may come before or after positional arguments (`kazari publish ./site --slug x` and `kazari publish --slug x ./site` are the same). `kazari help` lists the commands and `kazari <command> -h` a command's flags.

## Install

The release archive's binary is the client too; see [install.md](install.md#install-the-release-binary). On a laptop, `install -m 0755 kazari ~/.local/bin/kazari` is enough.

## Configuration

Each setting comes from the first of: the global flag, the environment variable, the config file. The environment variables may also be set in a `.env` file in the working directory (`KEY=VALUE` lines, as for the server); a variable already set in the process environment wins over the file.

| Flag            | Variable             | Config key    | Meaning                                       |
| --------------- | -------------------- | ------------- | --------------------------------------------- |
| `--url`         | `KAZARI_URL`         | `url`         | User API root, e.g. `https://api.art.example.com` |
| `--token`       | `KAZARI_TOKEN`       | `token`       | API key (`kzk_...`)                           |
| `--admin-url`   | `KAZARI_ADMIN_URL`   | `admin_url`   | Admin API root, e.g. `https://admin.art.example.com` |
| `--admin-token` | `KAZARI_ADMIN_TOKEN` | `admin_token` | Admin token, or an admin user's key with the `admin` scope |
| `--config`      |                      |               | Config file path                              |
| `--json`        |                      |               | Print the JSON response instead of text       |

Give the URLs without `/v1`; the client adds it. When no admin URL is set and the user URL starts with `api.`, the admin URL is derived as `admin.<base>`. When no admin token is set, the user token is tried on the admin host.

The config file is `$XDG_CONFIG_HOME/kazari/config.yaml`, else `~/.config/kazari/config.yaml`. It is optional unless named with `--config`, and an unknown key is an error:

```yaml
url: https://api.art.example.com
token: kzk_abcdefgh_...
admin_url: https://admin.art.example.com   # optional
admin_token: ""                            # optional
```

Keep it, and any `.env` holding a token, private (`chmod 600`): they hold credentials.

## Output and exit codes

By default commands print short human-readable text: a URL after a publish, aligned tables for lists. `--json` (as a global flag or after the command) prints the API's JSON response instead, pretty-printed, for scripts:

```sh
kazari --json get q3-report | jq -r '.artifact.published_version'
```

| Exit code | Meaning                                                                       |
| --------- | ----------------------------------------------------------------------------- |
| `0`       | Success                                                                       |
| `1`       | The command or the server failed; stderr shows `kazari: <code>: <message>`    |
| `2`       | Usage or configuration error (bad flag, missing URL or token, bad config file) |

## Artifact commands

### publish

```text
kazari publish <dir> --slug <slug> [--name N] [--description D] [--label L]
               [--visibility public|unlisted] [--meta key=value ...] [--update]
```

Creates an artifact from a directory and prints its URL. The directory is streamed as a tar.gz bundle: regular files only, symlinks skipped, and every file or directory whose name starts with a dot skipped except `.well-known`.

With `--update`, an existing slug gets a new version that **mirrors** the directory: files missing from the directory are deleted, and the call fails with `version_conflict` if someone else published in between. A missing slug is created as usual.

```sh
kazari publish ./site --slug q3-report --name "Q3 report" --meta team=finance
kazari publish ./site --slug q3-report --update --label "numbers fixed"
```

### update

```text
kazari update <slug> [dir] [--label L] [--expected N] [--keep-stale]
              [--name N] [--description D] [--meta key=value ...] [--delete path ...]
```

Adds a version from a directory and/or changes fields. Files in `dir` are added or replace the same paths; files you do not send are **kept** (use `--delete path`, or `publish --update`, to remove them). Without a directory it changes only the fields you name. `--expected N` fails with `version_conflict` unless `N` is the latest version. A new version clears the stale mark unless `--keep-stale` is given.

```sh
kazari update q3-report ./site --label "add appendix" --expected 2
kazari update q3-report --name "Q3 report (final)" --meta status=final
kazari update q3-report --delete drafts/notes.html
```

`--meta key=` sets an empty value; the CLI cannot delete a metadata key (send `null` through `PATCH`, see [api.md](api.md#change-fields-patch-artifactsslug)).

### get

```text
kazari get <slug> [--files]
```

Shows the record and its versions (the published one is marked `*`); `--files` adds each version's file list.

### list

```text
kazari list [--mine created|updated|any] [--meta key=value ...] [--stale include|exclude|only]
            [--deleted include|exclude|only] [--limit N] [--cursor C] [--all]
```

Lists the artifacts you can see, newest-updated first. When more pages exist, stderr prints the next `--cursor`; `--all` follows cursors and prints everything.

```sh
kazari list --mine any --stale only
kazari list --meta team=finance --all
```

### rm

```text
kazari rm <slug> [--hard]
```

Soft-deletes an artifact (restorable until the retention period ends); `--hard` purges it now. Both need a key with `artifacts:delete`.

### stale, unstale

```text
kazari stale <slug> [--grace 336h]
kazari unstale <slug>
```

`stale` starts the grace period after which the artifact is soft-deleted; the page keeps serving meanwhile. `--grace` is a Go duration (hours: `72h`, `336h`); without it the server's `lifecycle.default_grace` applies. `unstale` cancels it.

### visibility

```text
kazari visibility <slug> public|unlisted|unpublished
```

`unpublished` takes the page offline (`404`) without deleting anything; `public` or `unlisted` brings it back. `unlisted` artifacts are served but left out of other people's listings.

### rollback

```text
kazari rollback <slug> <version>
```

Serves an existing version again (or a newer one: it is a pointer move, not a copy).

### files cat

```text
kazari files cat <slug> <path> [--version N]
```

Writes one file of the published version, or of version `N`, to stdout.

```sh
kazari files cat q3-report index.html --version 1 > old-index.html
```

### me, keys, version

```text
kazari me        # handle, admin flag, scopes, credential source, storage used of quota
kazari keys      # your API keys: prefix, label, scopes, created, last used, expiry, revocation
kazari version   # version, commit, build date and Go version of this binary
```

## Admin commands

Every `admin` command uses the admin URL and token.

| Command                                                                 | Does                                                        |
| ----------------------------------------------------------------------- | ----------------------------------------------------------- |
| `admin users list`                                                      | List users                                                  |
| `admin users create <handle> [--email E] [--role member\|admin] [--quota 500MiB]` | Create a user (quota defaults to `limits.default_user_quota`) |
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

`--quota` takes bytes or a unit (`500MiB`, `2GB`, `0` for unlimited). `--scopes` is comma-separated from `artifacts:read`, `artifacts:write`, `artifacts:delete` and `admin` (admin users only); the default is the three artifact scopes. `--since` takes an RFC 3339 time or a Go duration meaning "that long ago" (`24h`).

```sh
kazari admin users create ana --email ana@example.com --quota 1GiB
kazari admin keys issue ana --label ci --scopes artifacts:read,artifacts:write --expires 2160h
kazari admin jobs run sweep
kazari admin audit --since 24h --slug q3-report
```

The CLI has no commands for linking identities or listing and revoking OAuth grants; use the admin API for those ([api.md](api.md#admin-api)).
