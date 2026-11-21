#!/usr/bin/env python3
"""Contrast check for the colours of the Kazari website and of the pages the server renders.

Reads the light-dark() tokens in site/assets/css/main.css and the 24 solar terms in
site/data/sekki.toml, and checks every colour that carries text against every ground it sits on,
in both themes. Then does the same for internal/theme/css/tokens.css, the dashboard's copy of the
tokens with the accent fixed at gentian: its text colours, its tinted notices, its buttons, and at
3:1 the marks that are not text (the focus ring, a field's outline, a usage bar on its track).
Prints the weakest pairs and exits 1 if any falls under its minimum.
Run from the repository root: python3 .claude/skills/site-design/check-colours.py
"""
import re
import sys
import tomllib
from pathlib import Path

MIN = 4.5
MIN_MARK = 3.0  # WCAG 1.4.11, for what is not text
GROUNDS = ["bg", "bg-soft", "surface", "well", "code-bg"]
# Text tokens; --season-mark is decoration only, so it is not checked.
TEXT = ["text", "text-soft", "muted", "season", "ai", "matsu", "fuji", "kuchiba", "beni", "hl-comment", "hl-name"]


def luminance(hex_colour):
    def channel(c):
        c = int(c, 16) / 255
        return c / 12.92 if c <= 0.04045 else ((c + 0.055) / 1.055) ** 2.4
    h = hex_colour.lstrip("#")
    r, g, b = (channel(h[i:i + 2]) for i in (0, 2, 4))
    return 0.2126 * r + 0.7152 * g + 0.0722 * b


def ratio(a, b):
    hi, lo = sorted((luminance(a), luminance(b)), reverse=True)
    return (hi + 0.05) / (lo + 0.05)


def mix(a, b, share):
    """color-mix(in srgb, a share, b)."""
    a, b = a.lstrip("#"), b.lstrip("#")
    return "#" + "".join(f"{round(int(a[i:i + 2], 16) * share + int(b[i:i + 2], 16) * (1 - share)):02x}" for i in (0, 2, 4))


def read_tokens(css):
    return {name: (light, dark) for name, light, dark in
            re.findall(r"--([\w-]+):\s*light-dark\((#[0-9a-fA-F]{6}),\s*(#[0-9a-fA-F]{6})\)", css)}


css = Path("site/assets/css/main.css").read_text()
tokens = read_tokens(css.split("/* The 24 solar terms")[0])
missing = [t for t in GROUNDS + TEXT if t not in tokens]
if missing:
    sys.exit(f"main.css: no light-dark() value for --{', --'.join(missing)}")

rows = []  # (ratio, minimum, description)
for theme, i in (("light", 0), ("dark", 1)):
    for t in TEXT:
        for g in GROUNDS:
            rows.append((ratio(tokens[t][i], tokens[g][i]), MIN, f"--{t} on --{g} ({theme})"))
    # The primary button: --bg text on a --text fill, --text-soft on hover.
    for fill in ("text", "text-soft"):
        rows.append((ratio(tokens["bg"][i], tokens[fill][i]), MIN, f"--bg text on --{fill} (primary button, {theme})"))

terms = tomllib.loads(Path("site/data/sekki.toml").read_text())["terms"]
for n, t in enumerate(terms):
    label = f"{n:2} {t['kanji']} {t['name']}"
    for g in GROUNDS:
        rows.append((ratio(t["light"], tokens[g][0]), MIN, f"{label}: light {t['light']} on --{g}"))
        rows.append((ratio(t["dark"], tokens[g][1]), MIN, f"{label}: dark {t['dark']} on --{g}"))

if len(terms) != 24:
    rows.append((0, MIN, f"sekki.toml has {len(terms)} terms, not 24"))

# The dashboard, sign-in, account and gate pages: internal/theme.
APP_GROUNDS = ["bg", "surface", "well", "code-bg"]
APP_TEXT = ["text", "text-soft", "muted", "season", "ai", "matsu", "fuji", "kuchiba", "beni"]
TONES = ["matsu", "kuchiba", "beni", "ai", "fuji", "text-soft"]  # the status colours, and a notice's tint
app = read_tokens(Path("internal/theme/css/tokens.css").read_text())
missing = [t for t in APP_GROUNDS + APP_TEXT + ["field-border", "rest", "season-mark"] if t not in app]
if missing:
    sys.exit(f"internal/theme/css/tokens.css: no light-dark() value for --{', --'.join(missing)}")
# What the site also defines carries over unchanged; only the season differs, held at gentian.
for name, value in app.items():
    if name in tokens and name not in ("season", "season-mark") and tokens[name] != value:
        rows.append((0, MIN, f"theme: --{name} is {value}, the site has {tokens[name]}"))
for name in ("season", "season-mark"):
    if tuple(v.lower() for v in app[name]) != ("#5b57a3", "#a5a4f7"):
        rows.append((0, MIN, f"theme: --{name} is {app[name]}, not gentian"))

for theme, i in (("light", 0), ("dark", 1)):
    c = {name: value[i] for name, value in app.items()}
    for t in APP_TEXT:
        for g in APP_GROUNDS:
            rows.append((ratio(c[t], c[g]), MIN, f"theme: --{t} on --{g} ({theme})"))
    for fill in ("text", "text-soft"):
        rows.append((ratio(c["bg"], c[fill]), MIN, f"theme: --bg text on --{fill} (primary button, {theme})"))
    # A notice: an 8% tint of its tone over --surface, with its title in the tone, body text and links.
    for tone in TONES:
        tint = mix(c[tone], c["surface"], 0.08)
        for t in (tone, "text", "season"):
            rows.append((ratio(c[t], tint), MIN, f"theme: --{t} on a --{tone} notice ({theme})"))
    # The danger button: --beni text and outline, an 8% --beni fill on hover.
    for g in ("bg", "surface"):
        rows.append((ratio(c["beni"], mix(c["beni"], c[g], 0.08)), MIN, f"theme: danger button on hover over --{g} ({theme})"))
        rows.append((ratio(c["beni"], c[g]), MIN_MARK, f"theme: danger button outline on --{g} ({theme})"))
    for g in APP_GROUNDS:
        rows.append((ratio(c["text"], c[g]), MIN_MARK, f"theme: focus ring on --{g} ({theme})"))
    # A field is told from the page by its outline: on the page, in a box (--bg fill) and read-only (--code-bg).
    for g in ("bg", "surface", "code-bg"):
        rows.append((ratio(c["field-border"], c[g]), MIN_MARK, f"theme: field outline on --{g} ({theme})"))
    for fill in ("text-soft", "kuchiba", "beni"):
        rows.append((ratio(c[fill], c["rest"]), MIN_MARK, f"theme: usage bar --{fill} on its --rest track ({theme})"))
    for tone in TONES:
        for g in ("bg", "surface"):
            rows.append((ratio(c[tone], c[g]), MIN_MARK, f"theme: status dot --{tone} on --{g} ({theme})"))

rows.sort(key=lambda r: r[0] / r[1])
failures = [r for r in rows if r[0] < r[1]]
for r, minimum, what in (failures or rows[:8]):
    print(f"{r:5.2f}  (min {minimum})  {what}")
print(f"{len(rows)} pairs checked, {len(failures)} under their minimum" + ("" if failures else "; the weakest are listed above"))
sys.exit(1 if failures else 0)
