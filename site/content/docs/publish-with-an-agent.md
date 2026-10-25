---
title: Publish with an agent
description: Connect any AI agent, in the cloud or self-hosted, to Kazari; ask for pages, change and roll them back, share them safely and tidy them away.
aliases: [/docs/publish-with-claude/]
---

# Publish with an agent

Ask your AI agent for a prototype, a report, a dashboard or a small site, and Kazari gives it an address on your organisation's domain. You do not need a repository, a build step or a hosting account. This guide is for anyone who works with an agent, whether you write code or not.

You need a Kazari instance. Your administrator will give you its address, which looks like `art.example.com` in the examples below. If you are the administrator, start with [Install](install.md).

## Connect your agent

Kazari is an MCP server at `https://mcp.art.example.com/` (your instance's `mcp.` address). Any agent that can use a remote MCP server over Streamable HTTP can connect, whether it runs in the cloud or on your own machine. It either signs you in through OAuth or sends an API key as an `Authorization: Bearer kzk_...` header.

### Cloud agents

Agents that run in a web or desktop app add Kazari as a custom connector (some apps call it a remote MCP server):

1. In your agent's settings, add a custom connector with the address `https://mcp.art.example.com/` and connect.
2. The app sends you to sign in. If your organisation has set up single sign-on, use your work account; otherwise Kazari asks for an API key, which your administrator can issue for you.
3. Kazari shows a consent page naming the app, your account and what it may do. Approve it once.

In claude.ai, for example, custom connectors are under **Settings → Connectors**, and on Team and Enterprise plans an owner may need to add the connector for the organisation before members can connect it. A cloud agent reaches Kazari from the internet, so your instance needs a public `https` address.

### Self-hosted agents

Agents that run on your own machine or servers, such as command-line coding agents, take the server in their configuration. Many take a JSON entry along these lines; check your agent's documentation for where it goes:

```json
{
  "mcpServers": {
    "kazari": {
      "url": "https://mcp.art.example.com/",
      "headers": { "Authorization": "Bearer kzk_..." }
    }
  }
}
```

Leave out `headers` to sign in through OAuth instead, if your agent supports it. Claude Code, for example, adds the server with one command and opens your browser to sign in the first time it uses it:

```sh
claude mcp add --transport http kazari https://mcp.art.example.com/
```

Your instance also serves a skill with its conventions, limits and recipes. Agents that load skills over MCP find it on their own; others can call the `get_guide` tool. To install it for all your projects in Claude Code, download it with your API key (or run `kazari skill` with the [CLI](cli.md#skill)):

```sh
mkdir -p ~/.claude/skills/kazari
curl -fsSL -H "Authorization: Bearer kzk_..." https://api.art.example.com/v1/skill \
  -o ~/.claude/skills/kazari/SKILL.md
```

## Ask for a page

Say what you want and that it should go on Kazari. Naming a slug is optional, but it decides the address and cannot be changed later.

| You say | What the agent does |
| ------- | ------------------- |
| "Build a clickable prototype of a three-step signup flow and publish it to Kazari as `signup-flow`." | Writes the page and calls `create_artifact`. |
| "Turn this CSV into a dashboard with a chart per region and publish it." | Builds the page with the data in it, picks a slug, publishes. |
| "Make the buttons larger on `signup-flow`. Keep everything else." | Reads the page and calls `edit_file` with only the changed lines: a new version. |
| "Add a pricing page to `signup-flow`." | Calls `update_artifact` with the new file; every other file carries over. |
| "What versions of `signup-flow` are there?" | Calls `get_artifact` and lists them with their addresses. |
| "Roll `signup-flow` back to version 2." | Calls `publish_version`. Version 3 is kept. |
| "Make `signup-flow` unlisted." | Calls `set_visibility`. |
| "Make `signup-flow` members-only and give me a link for the client that lasts two weeks." | Calls `set_visibility` with `members`, then `create_share_link` with `expires_in: 14d`, and gives you the link. |
| "Stop the client's link to `signup-flow`." | Calls `list_share_links`, then `revoke_share_link`. |
| "We're done with `signup-flow`. Retire it in a week." | Calls `mark_stale` with a seven-day grace period. |

The agent replies with the address, for example `Published https://signup-flow.art.example.com/ (version 1)`. Apps that support MCP Apps also show a card with links to the page and the version.

## Versions and addresses

Every publish creates a new version, and old versions are never changed or thrown away:

- `https://signup-flow.art.example.com/` shows the **published** version, usually the newest.
- `https://signup-flow--v2.art.example.com/` shows **version 2**, always.

Share the main address with people who should see the latest work. Put a version address in feedback, sign-off or a ticket when it matters exactly what someone looked at.

## Share safely

> [!IMPORTANT]
> A public or unlisted page opens for anyone who has its address, and addresses are easy to guess. Make anything that is not for the open web `members`.

- **Public** pages appear in the listings that other Kazari users see. **Unlisted** pages are left out of other people's listings but are still open to anyone with the link.
- **Members** pages open only for people who sign in to your Kazari instance. The first visit goes through your organisation's sign-in; after that, other members pages open at once for twelve hours. Members pages still appear in other members' listings.
- **Share links** open a members page for someone who cannot sign in, such as a client or an outside reviewer. Each has an expiry (a week unless you ask for longer) and can be revoked on its own. Anyone who has the link can open the page until then, so send it only to the people it is for.
- **Unpublished** takes a page offline at once (visitors get "not found") without deleting anything. Ask your agent to make it public, unlisted or members again to bring it back.
- Search engines are told not to index pages unless you ask for the page to be indexable.

## Tidy up

Pages you no longer need can retire themselves. Ask your agent to mark a page stale with a grace period ("retire it in two weeks"). The page keeps working until the grace period ends, then:

1. it is **soft-deleted**: offline, but it can be restored for 30 days (by default; ask your administrator);
2. it is **purged**: the files are gone, and old links say "gone" for 90 days;
3. the **slug is freed** and can be used again.

Changed your mind during the grace period? Ask your agent to unmark it, or publish a new version, which clears the mark too. Deleting a page outright is possible too; the agent should check with you before a permanent delete, and apps that support it ask you to confirm.

## What works in a page

Kazari serves static files: HTML, CSS, JavaScript, images, fonts and data files. Your agent learns these rules from the tool descriptions, but they help when you ask for something ambitious:

- **One `index.html` is enough.** It is served at the root address. Extra files (styles, scripts, images, JSON data) are fine, and links between them should be relative (`about.html`, not `/about.html`) so they also work on version addresses.
- **Libraries from the common CDNs work.** By default scripts may load from cdnjs, jsDelivr and unpkg, styles from Google Fonts, and images from any `https` address. Other sources are blocked by the browser.
- **Data travels with the page.** A page can `fetch` its own files, such as `data.json`, but not other websites or APIs. Put the data in the page or next to it.
- **No server-side code.** There is nowhere to run PHP, an API route or a database, and forms cannot save anything on the server. For a prototype, fake the behaviour in JavaScript.
- **Each page is its own site.** `localStorage` and cookies are private to the page, so a prototype can remember state without seeing anyone else's.
- **Single-page apps** that route in the browser need a setting so that every path serves `index.html`: ask your agent to set the metadata `kazari.spa` to `true`.
- **Size.** By default a version can hold up to 50 MiB and 2,000 files, and one file up to 25 MiB. Above about 2 MiB, the agent is told to send files in chunks, so large pages work too.

Your instance's limits and allowed sources may differ; the tool descriptions your agent sees always state the real ones.

## When something goes wrong

| Message | What it means |
| ------- | ------------- |
| `slug_taken` | Another page (or one deleted recently) has that name. Pick another. |
| `invalid_slug` | Slugs are 3 to 63 lower-case letters, digits and single hyphens, and some names (`api`, `mcp`, `admin`, `www` and others) are reserved. |
| `forbidden` | Your key or sign-in lacks the permission for that action, or the instance lets only owners change their pages. Ask your administrator. |
| `quota_exceeded` | You have used your storage allowance. Retire old pages or ask for more. |
| `version_conflict` | Someone else published in between. The agent fetches the latest version and tries again. |
| `anchor_mismatch`, `edit_not_found` | The page changed since the agent read it, or the text to change was not found. The agent reads the page again and redoes the edit. |
| `rate_limited` | Too many changes in a minute (60 by default, across MCP and the API). Wait for the time the message names. |
| The page says "not found" | It is unpublished, deleted, or the address has a typo. |
| A library or font does not load | Its source is not on the allowed list. Ask your agent to inline it or use one of the CDNs above. |
| "No access" when signing in | Your account is not allowed on this instance yet. Ask your administrator. |
