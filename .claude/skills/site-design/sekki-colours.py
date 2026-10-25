#!/usr/bin/env python3
"""Derive each solar term's text-safe colours for the Kazari website from its base colour.

For every term in site/data/sekki.toml, `light` is `base` darkened in OKLCH (same hue, chroma reduced
only to stay in sRGB) until it reaches 4.6:1 on every light ground, and `dark` is `base` lightened the
same way until it reaches 7:1 on every dark ground. The grounds are the light-dark() values of --bg,
--bg-soft, --surface and --code-bg in site/assets/css/main.css. A base that already passes is kept.

Prints old and new values; with --write, saves them to sekki.toml. Then run check-colours.py.
Run from the repository root: python3 .claude/skills/site-design/sekki-colours.py [--write]
"""
import math
import re
import sys
import tomllib
from pathlib import Path

LIGHT_MIN, DARK_MIN = 4.6, 7.0
GROUNDS = ["bg", "bg-soft", "surface", "code-bg"]


def lin(c): return c / 12.92 if c <= 0.04045 else ((c + 0.055) / 1.055) ** 2.4
def gam(c): return 12.92 * c if c <= 0.0031308 else 1.055 * c ** (1 / 2.4) - 0.055
def rgb(h): return [int(h.lstrip("#")[i:i + 2], 16) / 255 for i in (0, 2, 4)]
def hex_(c): return "#" + "".join(f"{round(min(1, max(0, gam(x))) * 255):02x}" for x in c)


def luminance(h):
    r, g, b = (lin(x) for x in rgb(h))
    return 0.2126 * r + 0.7152 * g + 0.0722 * b


def ratio(a, b):
    hi, lo = sorted((luminance(a), luminance(b)), reverse=True)
    return (hi + 0.05) / (lo + 0.05)


def to_oklch(h):
    r, g, b = (lin(x) for x in rgb(h))
    l, m, s = (math.copysign(abs(v) ** (1 / 3), v) for v in (
        0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b,
        0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b,
        0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b))
    L = 0.2104542553 * l + 0.7936177850 * m - 0.0040720468 * s
    a = 1.9779984951 * l - 2.4285922050 * m + 0.4505937099 * s
    b2 = 0.0259040371 * l + 0.7827717662 * m - 0.8086757660 * s
    return L, math.hypot(a, b2), math.atan2(b2, a)


def from_oklch(L, C, H):
    a, b = C * math.cos(H), C * math.sin(H)
    l, m, s = ((L + 0.3963377774 * a + 0.2158037573 * b) ** 3,
               (L - 0.1055613458 * a - 0.0638541728 * b) ** 3,
               (L - 0.0894841775 * a - 1.2914855480 * b) ** 3)
    return [4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s,
            -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s,
            -0.0041960863 * l - 0.7034186147 * m + 1.7076147010 * s]


def colour(L, C, H):
    while C > 0 and not all(-1e-6 <= x <= 1 + 1e-6 for x in from_oklch(L, C, H)):
        C -= 0.002
    return hex_(from_oklch(L, C, H))


def adjust(base, grounds, target, step):
    worst = lambda h: min(ratio(h, g) for g in grounds)
    if worst(base) >= target:
        return base
    L, C, H = to_oklch(base)
    while True:
        L += step
        if not 0 <= L <= 1:
            sys.exit(f"{base}: cannot reach {target}:1 at this hue")
        h = colour(L, C, H)
        if worst(h) >= target:
            return h


css = Path("site/assets/css/main.css").read_text().split("/* The 24 solar terms")[0]
tokens = {n: (l, d) for n, l, d in re.findall(r"--([\w-]+):\s*light-dark\((#[0-9a-fA-F]{6}),\s*(#[0-9a-fA-F]{6})\)", css)}
light_grounds = [tokens[g][0] for g in GROUNDS]
dark_grounds = [tokens[g][1] for g in GROUNDS]

path = Path("site/data/sekki.toml")
text = path.read_text()
blocks = text.split("[[terms]]")
for i, t in enumerate(tomllib.loads(text)["terms"]):
    light = adjust(t["base"], light_grounds, LIGHT_MIN, -0.002)
    dark = adjust(t["base"], dark_grounds, DARK_MIN, +0.002)
    print(f"{i:2} {t['kanji']} base {t['base']}  light {t['light']} -> {light}  dark {t['dark']} -> {dark}")
    b = re.sub(r'light = "#\w+"', f'light = "{light}"', blocks[i + 1], count=1)
    blocks[i + 1] = re.sub(r'dark = "#\w+"', f'dark = "{dark}"', b, count=1)
if "--write" in sys.argv[1:]:
    path.write_text("[[terms]]".join(blocks))
    print(f"wrote {path}")
