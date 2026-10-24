---
paths:
  - "docs/**"
  - "site/content/**"
---

# Docs pages

- `site/hugo.toml` mounts `docs/` as `content/docs`, so `docs/<name>.md` is `/docs/<name>/`. Those files stay GitHub-readable Markdown: no front matter, the first `# ` heading is the title, links between them are relative `.md` links (`install.md#tls-modes`), and links to `README.md` go to GitHub. A link to a missing page fails the build; a wrong `#fragment` does not, so keep anchors matching GitHub-style heading ids.
- The guides in `site/content/docs/` may use GitHub alerts (`> [!NOTE]`).
- A new page needs an entry in `site/data/nav.toml` (sidebar, previous/next order, search index).
