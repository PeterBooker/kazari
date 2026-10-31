---
name: docs-check
description: Find and fix statements in docs/, the website guides, the landing page and the Kazari skill that a code change has made stale.
disable-model-invocation: true
argument-hint: "[revision or range, default: uncommitted changes]"
---

# Docs check

Uncommitted changes:

!`git status --short`

Check the uncommitted changes (`git diff HEAD`), or the revision or range given as ARGUMENTS (`git show <rev>` for one commit, `git diff <range>` for a range).

1. From the Go and config changes, list every user-visible behaviour change: commands and flags, config keys and defaults, REST endpoints and fields, MCP tools and inputs, limits and quotas, error codes, metric names, scheduled jobs, headers. A change nobody outside the code can see needs nothing.
2. For each one, `grep -rn` the old and the new name or value across the doc set. Grep rather than reading documents whole; topics such as quotas and limits appear in many places.
   - Doc set: `docs/*.md`, `deploy/kazari.yaml` and `deploy/kazari.reference.yaml` (comments), `deploy/kazari.env.example`, `deploy/compose.yml` (header), `site/content/docs/*.md`, `site/layouts/home.html`, `README.md`, `site/static/install.sh` (its printed hints).
   - Main homes: CLI in `docs/cli.md`, config in `docs/configuration.md` and `deploy/kazari.reference.yaml`, install paths in `docs/install.md`, DNS, certificates and proxies in `docs/certificates.md`, REST in `docs/api.md`, MCP in `docs/mcp.md`, sign-in in `docs/identity.md`, metrics, jobs and quotas in `docs/ops.md`, limits in `docs/configuration.md`, contributor commands in `docs/development.md`.
   - `examples/kazari/SKILL.md` is generated from `internal/mcpserver/skill.go` and `describe.go`: never edit it by hand. If those or the defaults changed, run `go test ./internal/mcpserver -run TestExampleSkill -update` and include the result.
3. Fix each stale or missing statement in the style of the file around it. Change no code.
4. If anything under `docs/` or `site/` changed, `hugo --source site --gc --minify` must print no WARN or ERROR.
5. Report each behaviour change with the files you updated, and anything you were unsure of.
