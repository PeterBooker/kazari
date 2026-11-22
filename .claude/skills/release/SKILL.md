---
name: release
description: Prepare a Kazari release - write the version's CHANGELOG.md entry from the pull requests merged since the last tag, as prose for the people who run an instance or write agents and scripts against it, record the release's schema, and open the pull request that is merged before the tag. Use when asked to prepare, cut or plan a release, to write or update the changelog or release notes, or to say what changed since the last release.
---

# Preparing a release

A release is a pull request, then a tag. The pull request adds the version's section to `CHANGELOG.md` and records the release's database schema; once it is merged, the tag on the merge commit runs `release.yml`, which refuses a tag without a changelog section and publishes that section as the GitHub release notes (`.github/scripts/release-notes.sh`). The same file is the changelog page of the docs (`/docs/changelog/`, mounted in `site/hugo.toml`) and ships in the archives and packages.

You prepare the pull request. Tagging is the maintainer's: never push a tag.

## 1. Gather what changed

Start from the remote, not the working tree:

```sh
git fetch --tags origin
last=$(git describe --tags --abbrev=0 origin/main)
git log --reverse --format='%h %s' "$last"..origin/main
```

Most commits are squashed pull requests ending in `(#N)`. Read each one's title and description:

```sh
gh pr view N --json title,body,files
```

For a commit without a pull request, read its message with `git show -s`. Then read what the release changed for its readers, which the descriptions may summarise loosely:

```sh
git diff "$last"..origin/main -- docs/ deploy/ README.md internal/sqlite/migrations/
```

The docs diff is the most reliable source for names, defaults and upgrade steps, and `docs/ops.md` has an "Upgrading to X.Y" section when a release needs one. Do not describe a change you have not found in a description, a commit or a diff.

## 2. Choose the version

Ask the maintainer when they have not named it. Before 1.0, a release with new features or any change under **Upgrading** is a minor (0.7.0 → 0.8.0) and one with fixes only is a patch (0.7.0 → 0.7.1). From 1.0, a breaking change to the REST wire types, tool names and arguments, error codes, configuration keys or audit actions is a major.

## 3. Write the section

Add the section above the previous release, under the lede of `CHANGELOG.md`:

```markdown
## 0.8.0

Released 2026-12-01.

One paragraph on what the release is about, for someone deciding whether to upgrade.

### Upgrading

### New

### Changed

### Fixed
```

- The heading is the bare version, exactly: `release-notes.sh` matches `## <tag>`, and the docs link to it as `#080`.
- The date is the day the tag is planned, usually today.
- Leave out the lede for a release of one or two items, and any subsection with nothing in it. Keep the order.

What goes where:

- **Upgrading**: what someone must do or check before or after upgrading, and anything that can break a script, a client or a configuration: a renamed or removed key, flag, route, field or tool; a changed default; a new requirement; a migration that renames or rewrites data. Say what to do, and link the "Upgrading to X.Y" section of `docs/ops.md` when there is one.
- **New**: capabilities that did not exist: commands, tools, routes, configuration sections, pages of the dashboard.
- **Changed**: behaviour that differs without needing action: new limits, scopes, lifetimes, result shapes.
- **Fixed**: what used to go wrong and now works, said as the working behaviour.
- **Left out**: CI, tests, linters, refactors, comments, dependency updates without an effect, and the website's design. Keep what changes what ships: release files, image tags, signatures, supported platforms.

How it reads:

- The readers are the people who run an instance and the people who write agents and scripts against it. Write for them, not for contributors: no package names, function names, pull request numbers or commit hashes.
- One bullet per change a reader would recognise; merge pull requests that make one change, and split one that makes several. Significant bullets start with a bold sentence that states the change; small ones are plain.
- Name things as the reader types them, in code: `kazari admin views`, `analytics.retention`, `GET /v1/ready`, `edit_file`. Give defaults and limits with their values.
- Plain, present tense, no marketing: no "powerful", "seamless", "easily" or exclamation marks. The house style is the `docs-writer` skill.
- Link the docs with relative links from the repository root, `docs/ops.md#quotas`: GitHub resolves them, and the site's link hook turns them into pages. A link to a missing page fails the Hugo build; a wrong anchor does not, so check each anchor against the heading.

## Fixes that close a weakness

Many fixes tighten what someone could do with access they should not have. Describe each one by the behaviour it has now, the way the docs state it, and nothing more:

- Do not say what the old behaviour allowed, who could have used it, or what it exposed, and do not use words such as vulnerability, attack, exploit, leak or finding. The pull request may explain all of this; the changelog does not repeat it.
- "Members pages send `Cross-Origin-Opener-Policy: same-origin`", not why they must. "Disabling a user revokes the share links they created", not what a removed member could still read.
- When the behaviour is not worth stating on its own, fold the fix into a general line or leave it out.

## 4. Record the schema

When the release adds a migration under `internal/sqlite/migrations/`, record its schema in the same pull request:

```sh
go test ./internal/sqlite -run TestMigrateFromEveryRelease -dump-schema 0.8.0
```

It writes `internal/sqlite/testdata/schema/0.8.0.sql` from the migrations on the branch, so run it after the last migration of the release is merged.

## 5. Check

```sh
.github/scripts/release-notes.sh 0.8.0            # prints the section the release will publish
hugo --source site --gc --minify                  # no WARN or ERROR
go test ./internal/sqlite -run TestMigrateFromEveryRelease
```

Then read the rendered page at `site/public/docs/changelog/index.html` or with `hugo server --source site`, and follow each link.

## 6. Open the pull request

On a branch named for the version, such as `release-0.8.0`: one commit, "Prepare 0.8.0", with the changelog section and the schema. The pull request is titled "Prepare 0.8.0"; its description is one paragraph saying what the release carries and that it is tagged after the merge, then a bullet list of the files. Show the maintainer the section in the conversation as well, and say which changes you left out and why.

## Correcting a released entry

A released section can be corrected in a later pull request: the docs page follows `main`, and the GitHub release notes are changed by hand on the release page. Keep the version and date as released.
