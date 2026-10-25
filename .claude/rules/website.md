---
paths:
  - "site/**"
---

# Website (`site/`)

Hugo standard edition at `HUGO_VERSION` in `.github/workflows/pages.yml` (`go install github.com/gohugoio/hugo@v<version>`), no theme and no Node. Preview with `hugo server --source site` (<http://localhost:1313/>).

- Templates use Hugo's current layout names (`baseof.html`, `home.html`, `docs/page.html`, `_partials/`, `_markup/`); `_markup/render-link.html` resolves the `.md` links. Plain CSS in `assets/css/main.css` (colour tokens with `light-dark()`), plain JS in `assets/js/main.js`.
- `content/_index.md` is front matter only: the landing page is `layouts/home.html`, and `layouts/home.json` builds the search index.
- A `data/nav.toml` entry without a page fails the build.
- `_partials/head.html` writes every page's search and sharing metadata: title, description, canonical, Open Graph and X card with the social card image (`assets/images/social-card.png`, from `social-card.sh`), and schema.org JSON-LD from `_partials/schema.html` (WebSite, SoftwareApplication on the home page, TechArticle and BreadcrumbList on docs pages). The description comes from front matter, else `description` in `data/nav.toml` (`_partials/description.html`). `layouts/robots.txt` names the sitemap; `layouts/home.llms.txt` builds `/llms.txt` from `nav.toml`. Check the head of a built page after changing any of them.
- Link with `.RelPermalink`, `relURL` or `site.GetPage`, never a hand-written path starting with `/`: the base URL is `baseURL` in `hugo.toml` (`https://kazari.io/`), and `pages.yml` deliberately does not override it.
- How the site looks (colours that follow the solar terms in `data/sekki.toml`, type, layout, Japanese labels, the subset fonts from `subset-fonts.sh`) is the `site-design` skill: load it before any visual change.
