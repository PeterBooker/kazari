---
title: Publish with Claude
description: Connect claude.ai or Claude Code to Agalma, ask for pages, change and roll them back, share them safely and tidy them away.
---

# Publish with Claude

Ask Claude for a prototype, a report, a dashboard or a small site, and Agalma gives it an address on
your organisation's domain. You do not need a repository, a build step or a hosting account. This
guide is for anyone who works with Claude, whether you write code or not.

You need an Agalma instance. Your administrator will give you its address, which looks like
`art.example.com` in the examples below. If you are the administrator, start with [Install](install.md).

## Connect Claude

### claude.ai

Add Agalma as a custom connector:

1. In claude.ai, open **Settings → Connectors** and add a custom connector.
2. Enter the address `https://mcp.art.example.com/` (your instance's `mcp.` address) and connect.
3. Claude.ai sends you to sign in. If your organisation has set up single sign-on, use your work
   account; otherwise Agalma asks for an API key, which your administrator can issue for you.
4. Agalma shows a consent page naming claude.ai, your account and what it may do. Approve it once.

On Team and Enterprise plans, an owner may need to add the connector for the organisation before
members can connect it.

### Claude Code

```sh
claude mcp add --transport http agalma https://mcp.art.example.com/
```

The first time Claude Code uses the server it opens your browser to sign in, exactly as above. To use
an API key instead, add `--header "Authorization: Bearer agk_..."` to the command.

Agalma's repository also ships a Claude Code skill with its conventions and recipes. Install it for
all your projects with:

```sh
mkdir -p ~/.claude/skills/agalma
curl -fsSL https://raw.githubusercontent.com/PeterBooker/agalma/main/skills/agalma/SKILL.md \
  -o ~/.claude/skills/agalma/SKILL.md
```

### Other MCP clients

Any client that speaks MCP over Streamable HTTP can connect to `https://mcp.art.example.com/`, either
by signing in through OAuth or with an `Authorization: Bearer agk_...` header. Many clients take a
JSON entry along these lines; check your client's documentation for where it goes:

```json
{
  "mcpServers": {
    "agalma": {
      "url": "https://mcp.art.example.com/",
      "headers": { "Authorization": "Bearer agk_..." }
    }
  }
}
```

## Ask for a page

Say what you want and that it should go on Agalma. Naming a slug is optional, but it decides the
address and cannot be changed later.

| You say | What Claude does |
| ------- | ---------------- |
| "Build a clickable prototype of a three-step signup flow and publish it to Agalma as `signup-flow`." | Writes the page and calls `create_artifact`. |
| "Turn this CSV into a dashboard with a chart per region and publish it." | Builds the page with the data in it, picks a slug, publishes. |
| "Make the buttons larger on `signup-flow`. Keep everything else." | Calls `update_artifact` with only the changed files: a new version. |
| "What versions of `signup-flow` are there?" | Calls `get_artifact` and lists them with their addresses. |
| "Roll `signup-flow` back to version 2." | Calls `publish_version`. Version 3 is kept. |
| "Make `signup-flow` unlisted." | Calls `set_visibility`. |
| "We're done with `signup-flow`. Retire it in a week." | Calls `mark_stale` with a seven-day grace period. |

Claude always replies with the address, for example
`Published https://signup-flow.art.example.com/ (version 1)`.

## Versions and addresses

Every publish creates a new version, and old versions are never changed or thrown away:

- `https://signup-flow.art.example.com/` shows the **published** version, usually the newest.
- `https://signup-flow--v2.art.example.com/` shows **version 2**, always.

Share the main address with people who should see the latest work. Put a version address in
feedback, sign-off or a ticket when it matters exactly what someone looked at.

## Share safely

> [!IMPORTANT]
> Anyone who has the link can open a page. Agalma has no per-page passwords or sign-in for viewers
> yet, so do not publish passwords, personal data or anything confidential.

- **Public** pages appear in the listings that other Agalma users see. **Unlisted** pages are left
  out of other people's listings but are still open to anyone with the link.
- **Unpublished** takes a page offline at once (visitors get "not found") without deleting anything.
  Ask Claude to make it public or unlisted again to bring it back.
- Search engines are told not to index pages unless you ask for the page to be indexable.

## Tidy up

Pages you no longer need can retire themselves. Ask Claude to mark a page stale with a grace period
("retire it in two weeks"). The page keeps working until the grace period ends, then:

1. it is **soft-deleted**: offline, but it can be restored for 30 days (by default; ask your
   administrator);
2. it is **purged**: the files are gone, and old links say "gone" for 90 days;
3. the **slug is freed** and can be used again.

Changed your mind during the grace period? Ask Claude to unmark it, or publish a new version, which
clears the mark too. Deleting a page outright is possible too; Claude should check with you before a
permanent delete.

## What works in a page

Agalma serves static files: HTML, CSS, JavaScript, images, fonts and data files. Claude knows these
rules from the tool descriptions, but they help when you ask for something ambitious:

- **One `index.html` is enough.** It is served at the root address. Extra files (styles, scripts,
  images, JSON data) are fine, and links between them should be relative (`about.html`, not
  `/about.html`) so they also work on version addresses.
- **Libraries from the common CDNs work.** By default scripts may load from cdnjs, jsDelivr and
  unpkg, styles from Google Fonts, and images from any `https` address. Other sources are blocked by
  the browser.
- **Data travels with the page.** A page can `fetch` its own files, such as `data.json`, but not
  other websites or APIs. Put the data in the page or next to it.
- **No server-side code.** There is nowhere to run PHP, an API route or a database, and forms cannot
  save anything on the server. For a prototype, fake the behaviour in JavaScript.
- **Each page is its own site.** `localStorage` and cookies are private to the page, so a prototype
  can remember state without seeing anyone else's.
- **Single-page apps** that route in the browser need a setting so that every path serves
  `index.html`: ask Claude to set the metadata `agalma.spa` to `true`.
- **Size.** By default a version can hold up to 50 MiB and 2,000 files, and one file up to 25 MiB.
  Above about 2 MiB, Claude is told to send files in chunks, so large pages work too.

Your instance's limits and allowed sources may differ; the tool descriptions Claude sees always
state the real ones.

## When something goes wrong

| Message | What it means |
| ------- | ------------- |
| `slug_taken` | Another page (or one deleted recently) has that name. Pick another. |
| `invalid_slug` | Slugs are 3 to 63 lower-case letters, digits and single hyphens, and some names (`api`, `mcp`, `admin`, `www` and others) are reserved. |
| `forbidden` | Your key or sign-in lacks the permission for that action, or the instance lets only owners change their pages. Ask your administrator. |
| `quota_exceeded` | You have used your storage allowance. Retire old pages or ask for more. |
| `version_conflict` | Someone else published in between. Claude fetches the latest version and tries again. |
| `rate_limited` | Too many changes in a minute (60 by default, across Claude and the API). Wait for the time the message names. |
| The page says "not found" | It is unpublished, deleted, or the address has a typo. |
| A library or font does not load | Its source is not on the allowed list. Ask Claude to inline it or use one of the CDNs above. |
| "No access" when signing in | Your account is not allowed on this instance yet. Ask your administrator. |
