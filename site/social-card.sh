#!/usr/bin/env bash
# Renders the two images that represent the site outside it:
#   assets/images/social-card.png   1200x630 link preview (og:image), from social-card.html
#   static/apple-touch-icon.png     180x180 home screen icon: the mark on the light ground, inset
#                                   for the rounded mask (iOS fills transparency with black)
# Rerun it after changing social-card.html, the hero headline or the mark, and commit the PNGs.
# Needs chromium and rsvg-convert (pacman -S chromium librsvg).
set -euo pipefail
cd "$(dirname "$0")"

mkdir -p assets/images
chromium --headless --disable-gpu --hide-scrollbars --force-device-scale-factor=1 \
  --window-size=1200,630 --virtual-time-budget=5000 \
  --screenshot="$PWD/assets/images/social-card.png" "file://$PWD/social-card.html"

rsvg-convert -w 180 -h 180 -o static/apple-touch-icon.png <<'SVG'
<svg xmlns="http://www.w3.org/2000/svg" viewBox="-8 -9.75 48 48"><rect x="-8" y="-9.75" width="48" height="48" fill="#edefea"/><circle cx="16" cy="16" r="11" fill="none" stroke="#222621" stroke-width="1.8"/><path d="M9.87 2.86A14.5 14.5 0 0 1 22.13 2.86L19.17 9.2A7.5 7.5 0 0 0 12.83 9.2Z" fill="#5b57a3"/></svg>
SVG

file assets/images/social-card.png static/apple-touch-icon.png
