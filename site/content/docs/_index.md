---
title: Documentation
description: Start here. Pick a path for publishing with an AI agent, publishing from code, or running your own Kazari instance.
---

# Documentation

Kazari hosts static web pages. An AI agent, the CLI or the REST API publishes a page, and Kazari serves it at its own address on your domain, keeps every version, and retires it when you are done with it.

Pick the path that fits what you want to do:

<div class="paths">
  <a href="quickstart/"><strong>Try it on your machine</strong><span>Run Kazari locally with Docker and publish a first page in about five minutes.</span></a>
  <a href="publish-with-an-agent/"><strong>Make pages with an agent</strong><span>Connect any agent, in the cloud or self-hosted, ask for a page, change it, share it and tidy it away.</span></a>
  <a href="publish-from-code/"><strong>Publish from code and CI</strong><span>The CLI, preview deployments from GitHub Actions, and the REST API.</span></a>
  <a href="install/"><strong>Run it for your team</strong><span>Docker Compose, a package or the binary, DNS and the wildcard certificate, the first admin user, then sign-in.</span></a>
</div>

## Key ideas

Six terms come up on every page:

**Artifact** : One page or site. It has a name, a description, an owner and a list of versions.

**Slug** : The artifact's permanent short name, which becomes its address: the slug `q3-report` is served at `https://q3-report.art.example.com/`. A slug is 3 to 63 lower-case letters, digits and single hyphens. It cannot be renamed.

**Version** : An unchangeable snapshot of the artifact's files. Every publish adds one. Version 3 always lives at `https://q3-report--v3.art.example.com/`.

**Published version** : The version served at the artifact's main address. It is usually the newest, but rolling back points it at an older one without deleting anything.

**Visibility** : Who can find and open a page. It is one of four settings:

- `public` pages appear in everyone's listings.
- `unlisted` pages are left out of other people's listings.
- `members` pages open only for people who sign in to the instance, and for whoever holds one of the page's share links.
- `unpublished` takes the page offline without deleting it.

Public and unlisted pages are served to anyone who has the link.

**Stale** : A page marked for retirement. It keeps serving through a grace period. Then it is soft-deleted, which takes it offline but keeps it restorable. Later it is purged, and finally its slug becomes free again.

`art.example.com` stands for your instance's domain throughout these pages.

## Reference

The reference pages hold every detail:

| Page                                | What is in it                                                     |
| ----------------------------------- | ----------------------------------------------------------------- |
| [MCP server](mcp.md)                | The seventeen tools, editing files, the skill, the preview app, chunked uploads, errors |
| [CLI](cli.md)                       | Every `kazari` command and flag, output and exit codes            |
| [REST API](api.md)                  | The user and admin APIs, wire types, limits and error codes       |
| [Configuration](configuration.md)   | Every YAML key, `${}` environment references, `.env`, overrides   |
| [Sign-in and identity](identity.md) | OAuth for MCP clients, OpenID Connect providers, provisioning     |
| [Operations](ops.md)                | Data layout, backup, upgrades, logs, metrics, jobs, quotas, abuse |
