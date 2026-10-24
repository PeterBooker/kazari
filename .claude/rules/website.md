---
paths:
  - "site/**"
---

# Website (`site/`)

Hugo standard edition at `HUGO_VERSION` in `.github/workflows/pages.yml` (`go install github.com/gohugoio/hugo@v<version>`), no theme and no Node. Preview with `hugo server --source site` (<http://localhost:1313/kazari/>).

- Templates use Hugo's current layout names (`baseof.html`, `home.html`, `docs/page.html`, `_partials/`, `_markup/`); `_markup/render-link.html` resolves the `.md` links. Plain CSS in `assets/css/main.css` (colour tokens with `light-dark()`), plain JS in `assets/js/main.js`.
- `content/_index.md` is front matter only: the landing page is `layouts/home.html`, and `layouts/home.json` builds the search index.
- A `data/nav.toml` entry without a page fails the build.
- Link with `.RelPermalink`, `relURL` or `site.GetPage`, never a hand-written path starting with `/`: the base URL comes from `hugo.toml` and `pages.yml` can override it.
