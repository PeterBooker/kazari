---
name: site-design
description: The "Sekki" design system of the Kazari website (site/, kazari.io) - colour that follows the 24 solar terms of the old Japanese calendar, Shippori Mincho and IBM Plex type, vertical Japanese section labels, hairline-ruled columns. Use for any change to how the site looks or reads - a new landing section, docs component, page layout, CSS in site/assets/css/main.css, templates in site/layouts, colours, fonts, icons, the logo, Japanese text - and to review such a change.
---

# Kazari website: the Sekki design

*Kazari* (飾り) is a decoration set out for a season and put away after. The site follows the seasons literally: its accent colour is the colour of the current solar term (節気, *sekki*), one of 24 periods of about fifteen days in the old Japanese calendar, so it changes every two weeks or so. Today's term and colour come from `site/data/sekki.toml`. Everything else is quiet: a grey-green paper ground, sumi-ink text, heavy mincho headings over a plain sans, vertical Japanese labels beside sections, and columns divided by hairlines.

The audience is developers, designers and admin staff making prototypes with Claude, and the IT people who decide whether to run it. It must read as calm and trustworthy first, and Japanese second.

This replaced a design that looked like a default Claude artifact (warm cream, light serif, terracotta, small capitalised labels over every heading, rounded cards). Do not drift back: see "Never" at the end.

The site's mechanics (Hugo version, layout names, link rules, `nav.toml`) are in `.claude/rules/website.md`; this skill covers how it looks.

## Files

| File | What it holds |
| ---- | ------------- |
| `site/assets/css/main.css` | All styles. Run through Hugo as a template: font URLs and one `:root[data-sekki="N"]` block per solar term are generated from `data/sekki.toml`. |
| `site/data/sekki.toml` | The 24 terms: start date, kanji, reading, English name, motif and its colour in three forms (`base`, `light`, `dark`). |
| `site/layouts/_partials/sekki.html` | Index of the term in force at a given time, for `data-sekki` on `<html>` at build time (`baseof.html`). |
| `site/layouts/_partials/head.html` | Inline script before first paint: the remembered theme, and the term for the visitor's own date (the site is only rebuilt on push). Font preloads. |
| `site/layouts/_partials/sekki-wheel.html` | The year wheel in the hero, drawn at build time. |
| `site/layouts/_partials/logo.html`, `site/static/favicon.svg` | The mark: a ring with one segment. The favicon is static (gentian, the term the design was made in). |
| `site/layouts/home.html` | The landing page. |
| `site/layouts/_partials/docs.html`, `_markup/render-blockquote.html` | Docs layout (crumb, running head, sidebar, "On this page", pager) and callouts. |
| `site/data/nav.toml` | Sidebar groups; `ja` is each group's Japanese label for the running head. |
| `site/subset-fonts.sh`, `site/static/fonts/` | Self-hosted, subset woff2 files and their licences. |
| `.claude/skills/site-design/check-colours.py` | Contrast check for every text colour in both themes and all 24 terms. |

## Colour

All colours are tokens on `:root` written with `light-dark()`; `data-theme` on `<html>` forces a scheme. Components never use a literal colour (the only exceptions are the wheel's segment fills, which come from `sekki.toml`, the favicon, and the search dialog's dimmed backdrop).

Neutrals, *rikyū-nezumi*, a grey with green in it:

| Token | Light | Dark | Use |
| ----- | ----- | ---- | --- |
| `--bg` | `#eff1ec` | `#151815` | page |
| `--bg-soft` | `#e7eae3` | `#1a1d1a` | footer, user chat bubble |
| `--surface` | `#f9faf7` | `#1c201c` | the few raised things: search, prompt demo, version boxes |
| `--text` | `#222621` | `#e7eae4` | body and headings |
| `--text-soft` | `#4b5249` | `#b5bcb1` | ledes, secondary text |
| `--muted` | `#5d655a` | `#879084` | captions, meta, inactive TOC |
| `--border`, `--border-strong` | `#d5dad1`, `#bcc3b8` | `#2b302a`, `#3c423a` | hairlines; strong for inputs and boxes |
| `--code-bg` | `#e6e9e2` | `#111411` | code blocks and inline code |

The season (set per term; gentian until the `data-sekki` blocks take over):

- `--season`: text-safe accent. Links, the primary button's fill, kickers, the crumb, active states, step numbers. At least 4.6:1 in light, 6.2:1 in dark.
- `--season-mark`: decoration only. Dots, the short rule over docs `h2`, the border of vertical labels, the logo segment, text-link underlines. In light it is the term's `base` colour, which can be pale (sunflower, yuzu): **never put text in `--season-mark`**.
- `--on-season`: text on a `--season` fill (white in light, the dark ground in dark).
- `--season-soft`: a 10% tint of the season for quiet fills (Claude's chat bubble, the selected search result).

Fixed colours with traditional names, for meaning that must not change with the season:

| Token | Name | Use |
| ----- | ---- | --- |
| `--ai` | 藍 indigo | note callouts, syntax keywords, system hosts (`mcp.`) |
| `--matsu` | 松 pine | tip callouts, strings, the "copied" tick |
| `--fuji` | 藤 wisteria | important callouts, tags, version hosts (`--v3`) |
| `--kuchiba` | 朽葉 fallen leaf | warning callouts, numbers |
| `--beni` | 紅 safflower red | caution callouts |
| `--hl-comment`, `--hl-name` | | syntax comments, names |

A warning is always ochre and a caution always red; never use `--season` for a state or a severity.

### The solar terms

`data/sekki.toml` lists the terms from 立春 (Start of spring, 4 February) as index 0. Each has:

- `base`: the motif's colour (plum blossom, gentian, yuzu...), for the wheel and the light theme's marks.
- `light`: `base` darkened in OKLCH, same hue, until it reaches 4.6:1 on `bg`, `bg-soft` and `surface`.
- `dark`: `base` lightened until 6.2:1 on the dark grounds; also the dark theme's marks.

`main.css` turns each entry into a block that sets `--season`, `--season-mark`, `--sekki-turn` (the index, which turns the logo segment) and the strings `--sekki-kanji`, `--sekki-name`, `--sekki-dates`, `--sekki-note`. Text that names the season (header, wheel centre, wheel caption, footer) is an empty `<span class="sekki-kanji|sekki-name|sekki-dates|sekki-note">` whose `::before` shows the string, so it follows `data-sekki` without JavaScript. Add a new season label the same way; do not write a term's name into a template.

To change or add a colour: edit `sekki.toml`, then run `python3 .claude/skills/site-design/check-colours.py` from the repository root (exit 1 lists every pair under 4.5:1). Keep neighbouring terms visibly different on the wheel.

To see another season in the browser, run `document.documentElement.dataset.sekki = "11"` in the console (11 is 大暑, sunflower, the palest; 20 is 大雪, pine, the darkest).

## Type

| Role | Face | Weights | Where |
| ---- | ---- | ------- | ----- |
| Display | Shippori Mincho B1 (`--font-display`) | 600, 800 | headings, vertical labels, step numbers, sidebar group names, pager titles, all Japanese text (`[lang="ja"]`) |
| Body | IBM Plex Sans (`--font-sans`) | 400-700 variable, italic | everything else |
| Code | IBM Plex Mono (`--font-mono`) | 400, 500, italic | code, hostnames, slugs, versions |

Scale (desktop; the `clamp()`s shrink on phones):

- Hero `h1`: mincho 800, `clamp(2.4rem, 4.6vw, 3.6rem)`, line-height 1.2. Landing `h2`: mincho 800, up to 2.6rem. Docs `h1`: mincho 800, up to 2.8rem; docs `h2`: mincho 800, 1.6rem, with a 1.75rem rule in `--season-mark` above it; `h3`: mincho 600, 1.25rem; `h4`: sans 600.
- Body 16px/1.7. Lede 1.1rem in `--text-soft`. Captions and meta .8-.9rem.
- Mincho is for display sizes: never set running text or anything under about .9rem in it, except the vertical labels.
- No capitalised, letter-spaced labels ("eyebrows"). A kicker is sentence case, .85rem, weight 600, in `--season`.
- British spelling in copy (colour, licence), as in the rest of the project.

### Fonts are subset

The woff2 files in `site/static/fonts` hold Latin plus only the Japanese characters the site uses. **After adding any Japanese character anywhere in `site/` or `docs/`, run `site/subset-fonts.sh`** (needs `curl` and fonttools with brotli) and commit the changed woff2 files; a missing character silently falls back to a system font. The script downloads the sources from google/fonts at a pinned commit and also copies their OFL licences. Do not load fonts from Google or any other CDN.

## Layout and components

- Container 1160px (docs 1400px with a 240px sidebar and 210px "On this page" column). Radii 2-4px. No shadows except the search dialog.
- Hairlines (`--border`) divide sections and columns. Boxes are rare: the prompt demo, code blocks, version boxes, the search dialog. Everything else is ruled columns or plain text. No rounded cards with icons in a tinted square.

Landing page (`home.html`), top to bottom: hero, how it works, who it's for, addresses, publish your way (tabs), features, lifecycle, self-hosting, closing call to action.

- **Hero**: `.hero-mark` (飾り with かざり beside it, vertical, in `--season`), the headline, lede, one `.button-primary` and one `.text-link`, `.hero-points`, then the year wheel. The wheel shows the 24 terms in their `base` colours with 立春 at the top, lifts the current one (`scale(1.1)`, full opacity), and names it in the centre.
- **Section**: every landing section after the hero is `.section > .container.band`, with a `.tate` vertical Japanese label first and the content beside it. The label is decoration: `lang="ja" aria-hidden="true"`; the English `h2` carries the meaning. Under 640px it turns horizontal above the heading.
- **`.ruled`**: columns divided by vertical hairlines with a hairline above (`.ruled-2`, default 3, `.ruled-4`). Used for the steps, audiences and the two self-hosting columns. Items are flex columns, so a `.card-link` at the end sits at the bottom. It stacks to two and then one column with rules on top.
- **`.split`**: heading and text on the left, a list or diagram on the right (addresses, lifecycle).
- **Buttons**: `.button-primary` (`--season` fill) for the single main action of a view; `.button` (outlined) for a second action that needs weight; `.text-link` (underlined in `--season-mark`) for the quieter second action; `.card-link` with the arrow icon for "go to the page about this".
- **Lists**: short dashes in `--muted`; dots in `--season-mark` only for `.hero-points`.
- Icons: `partial "icon.html" "<name>"`, 24px stroked line icons in `currentColor` (features use `--season`). Add new ones to that partial in the same style.

Docs (`docs.html`):

- `.crumb`: the sidebar group above the `h1`, in `--season`.
- `.hashira`: the running head, set vertically down the outer edge of the text as in a Japanese book: the group's `ja` label from `nav.toml` in `--season`, then the group and page title. Hidden under 920px. A new sidebar group needs a `ja` label.
- Sidebar: group names in mincho; the current page in `--text` weight 600 with a `--season-mark` dot.
- Callouts (GitHub alerts in `site/content/docs/`): `.callout-note|tip|important|warning|caution`, a 2px top rule and an 8% tint of their fixed colour, the title prefixed with its Japanese word.
- Tables: hairlines only, a `--text-soft` rule under the header. Code: `--code-bg`, 1px border, copy button on hover.
- `.paths` (docs overview): two columns of links with a thick top rule that turns `--season-mark` on hover.

## Japanese text

Japanese is a typographic device here, used sparingly and always correctly: common words a Japanese reader would expect, never decorative nonsense. Each label sits beside English that says the same thing. Current vocabulary:

| Where | Japanese | Reading | Meaning |
| ----- | -------- | ------- | ------- |
| Hero, footer | 飾り / かざり | kazari | decoration on display: the name |
| How it works | 仕組み | shikumi | how it works |
| Who it's for | 使う人 | tsukau hito | the people who use it |
| Addresses | アドレス | adoresu | (web) address |
| Publish your way | 公開 | kōkai | publishing |
| Features | 機能 | kinō | features |
| Lifecycle | 片付け | katazuke | tidying up |
| Self-hosting | 運用 | un'yō | operation |
| Closing call to action | はじめる | hajimeru | to begin |
| 404 | 見つかりません | mitsukarimasen | not found |
| Docs groups | はじめに / 運用 / 参照 / 設計記録 | hajime ni / un'yō / sanshō / sekkei kiroku | introduction / operation / reference / design records |
| Callouts | 注 / ヒント / 重要 / 警告 / 注意 | chū / hinto / jūyō / keikoku / chūi | note / tip / important / warning / caution |
| Wheel | 春 夏 秋 冬 | haru natsu aki fuyu | the four seasons |

For a new label, prefer a word already in the table; otherwise choose a short, common one and say in the change that a Japanese speaker should confirm it. Mark it `lang="ja"`, and `aria-hidden="true"` when English beside it already says it. Then rerun `site/subset-fonts.sh`.

## Voice

Plain, direct and specific, from the reader's side: "Publish with Claude", not "Leverage our MCP integration". Short sentences, active voice, no em-dash asides, no "not X but Y", no exclamation marks. Name the real thing (`q3-dashboard`, `art.example.com`, `kazari publish`) rather than a placeholder. The season is mentioned lightly and never explained twice on a page.

## Checks before a change ships

1. `hugo --source site --gc --minify` prints no WARN or ERROR.
2. `python3 .claude/skills/site-design/check-colours.py` exits 0 if any colour changed.
3. `site/subset-fonts.sh` was run if any Japanese character was added.
4. Look at it with `hugo server --source site` (<http://localhost:1313/>): the home page and a docs page with a table, code and a callout, at desktop and phone widths (about 390px: nothing may scroll sideways except code and tables), in light and dark (the theme button), and in two other seasons (`dataset.sekki = "11"` and `"20"`).

## Never

- Warm cream backgrounds, terracotta or orange accents, a light display serif: the look this design replaced.
- Capitalised, letter-spaced eyebrow labels over headings; badges and pills as decoration.
- Rounded cards with a shadow and an icon in a tinted square; a coloured stripe down one side of a card.
- `--season` for warnings, errors or status; text in `--season-mark`.
- A literal colour in a component, or a colour defined only inside a dark-mode block.
- Japanese text that is not real, natural Japanese; Japanese without English beside it that carries the meaning.
- Fonts or scripts from a CDN; Node or a CSS framework; links written as paths starting with `/`.
