#!/usr/bin/env python3
"""Contrast check for the Kazari website's colours.

Reads the light-dark() tokens in site/assets/css/main.css and the 24 solar terms in
site/data/sekki.toml, and checks every colour that carries text against every ground it sits on,
in both themes. Prints the weakest pairs and exits 1 if any falls under 4.5:1.
Run from the repository root: python3 .claude/skills/site-design/check-colours.py
"""
import re
import sys
import tomllib
from pathlib import Path

MIN = 4.5
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


css = Path("site/assets/css/main.css").read_text()
root = css.split("/* The 24 solar terms")[0]
tokens = {name: (light, dark) for name, light, dark in
          re.findall(r"--([\w-]+):\s*light-dark\((#[0-9a-fA-F]{6}),\s*(#[0-9a-fA-F]{6})\)", root)}
missing = [t for t in GROUNDS + TEXT if t not in tokens]
if missing:
    sys.exit(f"main.css: no light-dark() value for --{', --'.join(missing)}")

rows = []  # (ratio, description)
for theme, i in (("light", 0), ("dark", 1)):
    for t in TEXT:
        for g in GROUNDS:
            rows.append((ratio(tokens[t][i], tokens[g][i]), f"--{t} on --{g} ({theme})"))
    # The primary button: --bg text on a --text fill, --text-soft on hover.
    for fill in ("text", "text-soft"):
        rows.append((ratio(tokens["bg"][i], tokens[fill][i]), f"--bg text on --{fill} (primary button, {theme})"))

terms = tomllib.loads(Path("site/data/sekki.toml").read_text())["terms"]
for n, t in enumerate(terms):
    label = f"{n:2} {t['kanji']} {t['name']}"
    for g in GROUNDS:
        rows.append((ratio(t["light"], tokens[g][0]), f"{label}: light {t['light']} on --{g}"))
        rows.append((ratio(t["dark"], tokens[g][1]), f"{label}: dark {t['dark']} on --{g}"))

if len(terms) != 24:
    rows.append((0, f"sekki.toml has {len(terms)} terms, not 24"))

rows.sort()
failures = [r for r in rows if r[0] < MIN]
for r, what in (failures or rows[:8]):
    print(f"{r:5.2f}  {what}")
print(f"{len(rows)} pairs checked, {len(failures)} under {MIN}:1" + ("" if failures else "; the weakest are listed above"))
sys.exit(1 if failures else 0)
