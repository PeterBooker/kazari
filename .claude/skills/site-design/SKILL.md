---
name: site-design
description: The "Ma" design system of the Kazari website (site/, kazari.io) - light and spacious grey-green grounds (Ma) with a dusk dark theme (Yūgure), colour that follows the 24 solar terms of the old Japanese calendar, Manrope in light and medium weights, Noto Sans JP, DM Mono labels, a faint 飾り behind the hero, a 24-bar season strip, alternating bands and pill buttons. Use for any change to how the site looks or reads - a new landing section, docs component, page layout, CSS in site/assets/css/main.css, templates in site/layouts, colours, fonts, icons, the logo, Japanese text - and to review such a change.
---

# Kazari website: the Ma design

*Kazari* (飾り) is a decoration set out for a season and put away after. The site follows the seasons literally: its accent colour is the colour of the current solar term (節気, *sekki*), one of 24 periods of about fifteen days in the old Japanese calendar, so it changes every two weeks or so. Today's term and colour come from `site/data/sekki.toml`.

Everything else is *ma* (間), the space between things: a pale grey-green ground, ink text, large headlines in a light sans with the end of the phrase a step heavier, generous bands of space instead of rules, and one faint, very large 飾り behind the hero. The dark theme is *Yūgure* (夕暮れ, dusk): the same page at dusk, with lighter, softer season colours. Only colour tokens differ between the two; layout and type are shared.

The audience is developers, designers and admin staff making prototypes with AI agents (any agent, in the cloud or self-hosted, not one vendor's), and the IT people who decide whether to run it. It must read as calm and trustworthy first, and Japanese second.

This replaced two designs, and must not drift back to either (see "Never" at the end): a default Claude artifact look (warm cream, light serif, terracotta, capitalised labels over every heading, rounded cards), then "Sekki" (heavy mincho headings, vertical Japanese labels beside every section, hairlines between sections, a colour wheel).

The site's mechanics (Hugo version, layout names, link rules, `nav.toml`) are in `.claude/rules/website.md`; this skill covers how it looks.

## Files

| File | What it holds |
| ---- | ------------- |
| `site/assets/css/main.css` | All styles. Run through Hugo as a template: font URLs and one `:root[data-sekki="N"]` block per solar term are generated from `data/sekki.toml`. |
| `site/data/sekki.toml` | The 24 terms: start date, kanji, reading, English name, motif and its colour in three forms (`base`, `light`, `dark`). |
| `site/layouts/_partials/sekki.html` | Index of the term in force at a given time, for `data-sekki` on `<html>` at build time (`baseof.html`). |
| `site/layouts/_partials/head.html` | Inline script before first paint: the remembered theme, and the term for the visitor's own date (the site is only rebuilt on push). Font preloads. |
| `site/layouts/_partials/sekki-strip.html` | The season strip under the hero: the current term, a caption and 24 bars. |
| `site/layouts/_partials/logo.html`, `site/static/favicon.svg` | The mark: a ring with one segment. The favicon is static (gentian, the term the design was made in). |
| `site/layouts/home.html` | The landing page. |
| `site/layouts/_partials/docs.html`, `_markup/render-blockquote.html` | Docs layout (crumb, sidebar, "On this page", pager) and callouts. |
| `site/data/nav.toml` | Sidebar groups; `ja` is each group's Japanese label for the crumb. |
| `site/subset-fonts.sh`, `site/static/fonts/` | Self-hosted, subset woff2 files and their licences. |
| `.claude/skills/site-design/sekki-colours.py` | Derives every term's `light` and `dark` from its `base` for the current grounds. |
| `.claude/skills/site-design/check-colours.py` | Contrast check for every text colour in both themes and all 24 terms. |

## Colour

All colours are tokens on `:root` written with `light-dark()`; `data-theme` on `<html>` forces a scheme. Components never use a literal colour (the only exceptions are the strip's per-term hover colours, which come from `sekki.toml`, the favicon, and the search dialog's dimmed backdrop).

Neutrals, a grey with green in it:

| Token | Ma (light) | Yūgure (dark) | Use |
| ----- | ---------- | ------------- | --- |
| `--bg` | `#edefea` | `#191c1a` | page |
| `--bg-soft` | `#f6f7f3` | `#1f2320` | alternate landing bands (`.section-soft`) |
| `--surface` | `#f6f7f3` | `#1f2320` | boxes: hosts, prompt demo, docs paths, search, copy button |
| `--well` | `#e6e9e2` | `#282d29` | the user's chat bubble, icon-button hover |
| `--text` | `#222621` | `#e4e7e1` | body, headings, the primary button's fill |
| `--text-soft` | `#50574d` | `#a9b0a5` | ledes, secondary text, nav |
| `--muted` | `#5e655b` | `#8e968a` | captions, mono labels, Japanese labels, inactive TOC |
| `--border`, `--border-strong` | `#c9cfc4`, `#adb5a8` | `#2e332f`, `#444b45` | hairlines; strong for inputs, outlined buttons, table heads |
| `--box-border` | transparent | `#2e332f` | box outlines: light boxes are fills, dark boxes are outlines |
| `--code-bg` | `#e4e7df` | `#141715` | code blocks and inline code |
| `--ghost` | `#dee2d9` | `#232724` | the large 飾り behind the hero; decoration only, never text |
| `--rest` | `#c9cfc4` | `#363c37` | the season strip's bars at rest |

A box is always `background: var(--surface); border: 1px solid var(--box-border)`, so it is a lighter fill in Ma and an outline in Yūgure. Inside `.section-soft`, `--surface` becomes `--bg`, so a box there reads as a slightly darker well.

The season (set per term; gentian until the `data-sekki` blocks take over):

- `--season`: text-safe accent. Links, `.card-link`s, step numbers, the season kanji in the strip and the crumb, slugs in hostnames, active TOC entries. At least 4.6:1 in Ma, 7:1 in Yūgure.
- `--season-mark`: decoration only. Dots (hero kicker, header and footer season labels, current sidebar page), the current bar in the strip, the short rule over docs `h2`, the active tab underline, the logo segment, the lifecycle markers, a text link's underline on hover. In Ma it is the term's `base` colour, which can be pale (sunflower, yuzu): **never put text in `--season-mark`**.
- `--season-soft`: a 10% tint of the season for quiet fills (the selected search result).

The season is used sparingly: a few marks and links per screen. Nothing is ever filled with it behind text; the primary button is ink (`--text`) with `--bg` text.

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

- `base`: the motif's colour (plum blossom, gentian, yuzu...), for the light theme's marks and a bar's hover colour.
- `light`: `base` darkened in OKLCH, same hue, until it reaches 4.6:1 on `bg`, `bg-soft`, `surface` and `code-bg`.
- `dark`: `base` lightened until 7:1 on the dark grounds; also the dark theme's marks. These come out soft and pale (gentian `#a5a4f7`), as Yūgure wants.

`main.css` turns each entry into a block that sets `--season`, `--season-mark`, `--sekki-turn` (the index, which turns the logo segment and shapes the strip) and the strings `--sekki-kanji`, `--sekki-name`, `--sekki-dates`, `--sekki-note`. Text that names the season (header, strip, footer) is an empty `<span class="sekki-kanji|sekki-name|sekki-dates|sekki-note">` whose `::before` shows the string, so it follows `data-sekki` without JavaScript. Add a new season label the same way; do not write a term's name into a template.

To change a term's colour or any ground (`--bg`, `--bg-soft`, `--surface`, `--code-bg`), run from the repository root:

1. `python3 .claude/skills/site-design/sekki-colours.py --write` to rederive every `light` and `dark` (it reads the grounds from `main.css`; without `--write` it only prints).
2. `python3 .claude/skills/site-design/check-colours.py`: exit 1 lists every pair under 4.5:1. It covers the neutrals and fixed colours on every ground (including `--well`), all 24 terms, and the primary button.

Keep neighbouring terms visibly different in the strip's hover colours.

To see another season in the browser, run `document.documentElement.dataset.sekki = "11"` in the console (11 is 大暑, sunflower, the palest; 20 is 大雪, pine, the darkest).

## Type

| Role | Face | Weights | Where |
| ---- | ---- | ------- | ----- |
| Sans (`--font-sans`) | Manrope | 300-700 variable | headings and body; no italic exists, so `<em>` in templates is set upright |
| Japanese (`--font-ja`) | Noto Sans JP | 200-700 variable | every `[lang="ja"]` element |
| Mono (`--font-mono`) | DM Mono | 400, 500, italic | code, hostnames, slugs, versions, and the small labels below |

Headlines are light with an accent of weight: landing `h1` and `h2`, the closing call to action and the 404 heading are Manrope 300 with the end of the phrase in `<b>` at 500 (`Ask Claude for a page. <b>Share it on your domain.</b>`). Pick a split where the second part completes the thought; keep the words unchanged.

Scale (desktop; the `clamp()`s shrink on phones):

- Hero `h1`: 300/500, `clamp(2.5rem, 6vw, 4.75rem)`, line-height 1.04, letter-spacing -.03em, at most 12em wide. Landing `h2`: 300/500, `clamp(1.9rem, 3.8vw, 2.9rem)`, line-height 1.1, -.025em, at most 14em.
- Column `h3` (`.ruled`): 500, 1.3rem. Feature `h3`: 600, 1.05rem. Step numbers: 300, 2.5rem, in `--season`.
- Docs `h1`: 300, up to 3rem. Docs `h2`: 500, 1.55rem, with a 1.5rem rule in `--season-mark` above it. `h3`: 600, 1.15rem. `h4`: 600, 1rem.
- Body 16px/1.7. Hero lede 1.19rem and section ledes 1.06rem in `--text-soft`. Captions .85-.9rem.
- Small labels are DM Mono, about .78rem, letter-spacing .04em, `--muted`, sentence case: `.kicker` (audiences), sidebar and TOC group names, footer column heads, pager labels, the crumb, the hero kicker and hero points. Never uppercase. The `KAZARI` wordmark (Manrope 500, .14em) is the only capitalised text.
- Japanese: `.ja-label` above a section heading is Noto Sans JP 400, .95rem, letter-spacing .3em, `--muted`. A season kanji is 500 in `--season`. The hero's 飾り is 200.
- British spelling in copy (colour, licence), as in the rest of the project.

### Fonts are subset

The woff2 files in `site/static/fonts` hold Latin plus only the Japanese characters the site uses. **After adding any Japanese character anywhere in `site/` or `docs/`, run `site/subset-fonts.sh`** (needs `curl` and fonttools with brotli) and commit the changed woff2 files; a missing character silently falls back to a system font. The script downloads the sources from google/fonts at a pinned commit, cuts the variable fonts to the weights above and copies their OFL licences. A weight outside those ranges needs the script changed first. Do not load fonts from Google or any other CDN.

## Layout and components

- Container 1240px with a 2.5rem gutter (1.5rem under 920px, 1rem under 640px); docs 1400px with a 240px sidebar and a 210px "On this page" column. Sticky 72px header (60px on phones), borderless on the home page.
- Space and bands separate sections, not rules: landing sections have 6.5rem of padding and alternate between `--bg` and `.section-soft`. Hairlines (`--border`) stay inside a section: ruled columns, lists, tables, the footer's top edge.
- Radii: 999px for buttons, the search field, chips and version pills; 18px for the prompt demo and search dialog; 14px for boxes, callouts and pager links; 12px for code blocks and chat bubbles; 2px for the strip's bars. No shadows except the search dialog.

Landing page (`home.html`), top to bottom: hero, season strip, how it works (soft), who it's for, addresses (soft), publish your way, features (soft), lifecycle, self-hosting (soft), closing call to action. A new section keeps the alternation.

- **Hero**: `.hero-ghost` (飾り, very large, `--ghost`, `aria-hidden`, behind the text at the right), `.hero-kicker` (mono, with a `--season-mark` dot), the headline, `.lede`, one `.button-primary` and one `.text-link`, then `.hero-points` (mono).
- **Season strip** (`sekki-strip.html`, `id="season"`): the current term's kanji in `--season`, its name and dates, the caption on the right, then 24 bars from 立春 with each term's kanji below. The current bar is tall and in `--season-mark`; the others are `--rest` and shorter the further they are from it (`--i` against `--sekki-turn`, in CSS). Hovering a bar shows that term's own colour. Under 760px only the current term's kanji stays, so nothing scrolls sideways.
- **Section**: `.section[.section-soft] > .container > .section-head` with an `hgroup` (`.ja-label`, then the `h2`), and an optional lede `p` beside it at the right, aligned to the heading's foot. The Japanese label is decoration: `lang="ja" aria-hidden="true"`; the English `h2` carries the meaning. `.split` puts the head with its paragraphs on the left and a list or boxes on the right (addresses, lifecycle).
- **`.ruled`**: columns divided by vertical hairlines under one rule across the top (`.ruled-2`, default 3, `.ruled-4`). Used for the steps, audiences and the two self-hosting columns. Items are flex columns, so a `.card-link` at the end sits at the bottom. It stacks to two and then one column with a rule above each.
- **Buttons**: `.button-primary` (ink pill: `--text` fill, `--bg` text) for the single main action of a view; `.button` (outlined pill) for a second action that needs weight; `.text-link` (underlined in the text colour, the underline turns `--season-mark` on hover) for the quieter second action; `.card-link` (`--season` with the arrow icon) for "go to the page about this".
- **Boxes**: rare and quiet: hostnames, the prompt demo, code, docs paths, the pager. A box holds content, never decoration: no icons in tinted squares, no shadows.
- **Lists**: short dashes in `--muted`.
- Icons: `partial "icon.html" "<name>"`, 24px stroked line icons in `currentColor` (features use `--muted`). Add new ones to that partial in the same style.

Docs (`docs.html`):

- `.crumb`: the group's `ja` label from `nav.toml` in `--season`, then the group name in mono. A new sidebar group needs a `ja` label.
- Sidebar: group names as mono labels; the current page in `--text` weight 600 with a `--season-mark` dot. "On this page" marks the section being read in `--season`.
- Callouts (GitHub alerts in `site/content/docs/`): `.callout-note|tip|important|warning|caution`, a 14px-radius box with an 8% tint of their fixed colour, the title in that colour prefixed with its Japanese word.
- Tables: hairlines only, a `--border-strong` rule under the header. Code: `--code-bg`, 12px radius, copy button on hover.
- `.paths` (docs overview): two columns of boxed links that gain a `--border-strong` outline on hover.

## Japanese text

Japanese is a typographic device here, used sparingly and always correctly: common words a Japanese reader would expect, never decorative nonsense. Each label sits beside English that says the same thing. Current vocabulary:

| Where | Japanese | Reading | Meaning |
| ----- | -------- | ------- | ------- |
| Hero, footer | 飾り | kazari | decoration on display: the name |
| How it works | 仕組み | shikumi | how it works |
| Who it's for | 使う人 | tsukau hito | the people who use it |
| Addresses | アドレス | adoresu | (web) address |
| Publish your way | 公開 | kōkai | publishing |
| Features | 機能 | kinō | features |
| Lifecycle | 片付け | katazuke | tidying up |
| Self-hosting | 運用 | un'yō | operation |
| Closing call to action | はじめる | hajimeru | to begin |
| 404 | 見つかりません | mitsukarimasen | not found |
| Docs groups | はじめに / 運用 / 参照 | hajime ni / un'yō / sanshō | introduction / operation / reference |
| Callouts | 注 / ヒント / 重要 / 警告 / 注意 | chū / hinto / jūyō / keikoku / chūi | note / tip / important / warning / caution |
| Season strip, header, footer | the 24 term names (立春 ... 大寒) | from `sekki.toml` | the solar terms |

For a new label, prefer a word already in the table; otherwise choose a short, common one and say in the change that a Japanese speaker should confirm it. Mark it `lang="ja"`, and `aria-hidden="true"` when English beside it already says it. Then rerun `site/subset-fonts.sh`.

## Voice

Plain, direct and specific, from the reader's side: "Publish with an agent", not "Leverage our MCP integration". Say "your agent" or "an AI agent", not a product name; name a product only as one example among others, as the agent guide does with claude.ai and Claude Code. Short sentences, active voice, no em-dash asides, no "not X but Y", no exclamation marks. Name the real thing (`q3-dashboard`, `art.example.com`, `kazari publish`) rather than a placeholder. The season is mentioned lightly and never explained twice on a page.

## Checks before a change ships

1. `hugo --source site --gc --minify` prints no WARN or ERROR.
2. `python3 .claude/skills/site-design/check-colours.py` exits 0 if any colour changed (after `sekki-colours.py --write` if a ground or a `base` changed).
3. `site/subset-fonts.sh` was run if any Japanese character was added.
4. Look at it with `hugo server --source site` (<http://localhost:1313/>): the home page and a docs page with a table, code and a callout, at desktop and phone widths (about 390px: nothing may scroll sideways except code, tables and the tab list), in Ma and Yūgure (the theme button), and in two other seasons (`dataset.sekki = "11"` and `"20"`).

## Never

- Warm cream backgrounds, terracotta or orange accents, a light display serif: the first look this design replaced.
- Heavy mincho headings, vertical Japanese labels, hairlines between landing sections, a colour wheel: the Sekki look it replaced.
- Capitalised, letter-spaced eyebrow labels over headings (the `KAZARI` wordmark is the one exception); badges and pills as decoration.
- Rounded cards with a shadow and an icon in a tinted square; a coloured stripe down one side of a card or callout.
- A `--season` fill behind text; `--season` for warnings, errors or status; text in `--season-mark` or `--ghost`.
- A literal colour in a component, or a colour defined only inside a dark-mode block.
- Japanese text that is not real, natural Japanese; Japanese without English beside it that carries the meaning.
- Fonts or scripts from a CDN; Node or a CSS framework; links written as paths starting with `/`.
