---
paths:
  - "docs/**"
  - "site/content/**"
---

# Docs pages

- `site/hugo.toml` mounts `docs/` as `content/docs`, so `docs/<name>.md` is `/docs/<name>/`, except `security.md` and `adr/`, which the mount's `files` setting keeps off the site (they stay on GitHub, so no page on the site may link to them). Those files stay GitHub-readable Markdown: no front matter, the first `# ` heading is the title, links between them are relative `.md` links (`install.md#tls-modes`), and links to `README.md` go to GitHub. A link to a missing page fails the build; a wrong `#fragment` does not, so keep anchors matching GitHub-style heading ids.
- Pages in both places may use GitHub alerts (`> [!NOTE]`): GitHub renders them, and so does the site's `_markup/render-blockquote.html`.
- How a page reads (order, headings, paragraphs, code blocks, callouts) is the `docs-writer` skill.
- A new page needs an entry in `site/data/nav.toml` (sidebar, previous/next order, search index, `/llms.txt`). A page in `docs/` also needs a `description` there, under about 155 characters: it is the search snippet and link preview text, which the guides in `site/content/docs/` set in front matter instead.
