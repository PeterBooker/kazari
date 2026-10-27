# ADR 0004: Server-side fetch for files from URLs and page imports

Status: accepted (2026-10-27)

## Context

Agents that work in a chat interface cannot move bytes and cannot read a web page's markup: their fetch tools return the page as text, and an image or font never passes through the model. Two requests fail for that reason: "put this photo in the page", and "copy our site but change the hero". The agent rewrites a page from a text summary, references resources it cannot host, and never learns that the copy lost its stylesheet.

Kazari is the only party in the loop with a network stack, a filesystem and no token budget. An outbound fetch already exists for OAuth client ID metadata documents, dialling public addresses only.

A prototype against real pages (a Hugo site with minified markup and subresource integrity, a large public site with 39 assets) produced copies that render identically to the originals, in under a second each, once the stylesheet's `integrity` attribute was dropped with the rewrite that invalidated it.

## Decision

- **One fetch policy**, `internal/fetch`, shared with the CIMD fetch: public unicast addresses only, checked on the resolved address of every hop; at most five redirects; no credentials and no cookies; a fixed `User-Agent`; one timeout per request (`fetch.timeout`, 15 s) and one budget per import (`fetch.budget`, 60 s); bodies capped at the file limit and spilled to `data_dir/tmp`, never held in memory for the duration of a publish. `fetch.enabled: false` turns both features off and removes the tool.
- **A file may name a `url` instead of `content`** in `files[]` of a create or update, on the REST API and over MCP. The server downloads it before the publish; a failed download fails the whole call and publishes nothing.
- **`import_page`** (REST `POST /artifacts` with `source_url`, CLI `kazari import`) takes a snapshot of one page as a new artifact: the HTML with its subresources (stylesheets with their `url()` and `@import` references, scripts if kept, images, `srcset` candidates, media, icons, preloads, fonts) localised under `assets/` and referenced relatively; navigation links and form actions made absolute to the source; `<base>`, `meta refresh`, the page's own CSP, manifest, canonical and alternate links dropped; `integrity` and `crossorigin` dropped from localised elements; the page transcoded to UTF-8 with a `<meta charset>` to match. A resource stays remote only when the instance CSP names its host explicitly, so an allow-listed CDN keeps serving it and a `https:` wildcard does not turn into hot-linking.
- **Scripts are removed by default** (`keep_scripts` keeps them): the copy is meant to be static, and the source's analytics must not run on the instance's domain. Event-handler attributes go with them.
- **Formatting never creates whitespace where none was.** Existing whitespace-only text nodes become a newline and indentation; new ones are inserted only inside `<head>` and between the direct children of `<html>` and `<body>`, never inside `pre`, `textarea`, `script` or `style`. A minified page therefore stays on few lines, which is why the result carries an outline: each landmark's line, its opening tag as text for a text edit, and its first heading.
- **Synchronous**, with the budget as the bound. The measured cost of a sequential fetch is well under a second for typical pages, and an asynchronous job would add state for no gain.
- **No headless browser.** Kazari stays one static binary. A page that renders with JavaScript imports as a shell, and the result says so; a page behind a login or bot protection fails with `fetch_failed`, and the next step points at saving it from a browser and publishing the files.

## Consequences

- A member can make the server request any public URL, within the limits above. Trust boundary 4 in the security guide widens accordingly, every import is audited with its source, and imports count against the write rate like any publish.
- `golang.org/x/net/html` becomes a direct dependency; it is pure Go.
- An imported page is the member's copy, published under the instance's domain. The documentation says to import pages one has the right to reuse; Kazari does not check.
- Importing into an existing slug, multi-page imports and a text search over large files are not part of this change.
