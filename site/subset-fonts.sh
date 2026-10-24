#!/usr/bin/env bash
# Builds the self-hosted web fonts in site/static/fonts from the google/fonts repository at one commit:
#   Shippori Mincho B1 600 and 800  display; Latin plus every Japanese character the site uses
#   IBM Plex Sans 400-700, italic   body; one variable file each, Latin
#   IBM Plex Mono 400, 500, italic  code; Latin
# Rerun it after adding Japanese text anywhere in site/ or docs/: characters missing from the subset
# fall back to a system font. Needs curl and fonttools with brotli (pacman -S python-fonttools
# python-brotli, or pip install fonttools brotli). Sources are cached in ~/.cache/kazari-fonts.
set -euo pipefail
cd "$(dirname "$0")"

rev=e7db6ca603fd4862010383ff17ffd6cc4ffac1c0
base="https://raw.githubusercontent.com/google/fonts/$rev/ofl"
cache="${XDG_CACHE_HOME:-$HOME/.cache}/kazari-fonts/$rev"
out=static/fonts
mkdir -p "$cache" "$out"

fetch() { # family-dir file [saved-name]
  local dest="$cache/${3:-$2}"
  [[ -s $dest ]] || curl -fsSL --globoff -o "$dest" "$base/$1/$2"
}
fetch shipporiminchob1 ShipporiMinchoB1-SemiBold.ttf
fetch shipporiminchob1 ShipporiMinchoB1-ExtraBold.ttf
fetch shipporiminchob1 OFL.txt OFL-shippori.txt
fetch ibmplexsans 'IBMPlexSans%5Bwdth,wght%5D.ttf' IBMPlexSans-VF.ttf
fetch ibmplexsans 'IBMPlexSans-Italic%5Bwdth,wght%5D.ttf' IBMPlexSans-Italic-VF.ttf
fetch ibmplexsans OFL.txt OFL-plex-sans.txt
fetch ibmplexmono IBMPlexMono-Regular.ttf
fetch ibmplexmono IBMPlexMono-Medium.ttf
fetch ibmplexmono IBMPlexMono-Italic.ttf
fetch ibmplexmono OFL.txt OFL-plex-mono.txt

# Latin, punctuation and arrows for every face, plus each character outside ASCII that the site and
# docs use; a subset keeps only the characters its font has.
latin="U+0020-007E,U+00A0-017F,U+0192,U+02C6,U+02DA,U+02DC,U+2000-206F,U+20AC,U+2122,U+2190-2199,U+2212,U+2215,U+25CF,U+FEFF,U+FFFD"
text="$cache/text.txt"
LC_ALL=C.UTF-8 grep -rhoP '[^\x{00}-\x{7F}]' layouts data content assets ../docs | sort -u | tr -d '\n' > "$text"

subset() { # source output [pyftsubset options]
  pyftsubset "$1" --unicodes="$latin" --text-file="$text" --layout-features='*' \
    --drop-tables+=meta --flavor=woff2 --output-file="$out/$2" "${@:3}"
}

# The display face keeps the vertical-writing features (vert, vrt2) for the vertical labels.
subset "$cache/ShipporiMinchoB1-SemiBold.ttf" shippori-mincho-b1-600.woff2
subset "$cache/ShipporiMinchoB1-ExtraBold.ttf" shippori-mincho-b1-800.woff2

# Plex Sans is variable in width and weight: pin the width, keep weights 400 to 700.
fonttools varLib.instancer -q "$cache/IBMPlexSans-VF.ttf" wdth=100 wght=400:700 -o "$cache/plex-sans.ttf"
fonttools varLib.instancer -q "$cache/IBMPlexSans-Italic-VF.ttf" wdth=100 wght=400:700 -o "$cache/plex-sans-italic.ttf"
subset "$cache/plex-sans.ttf" ibm-plex-sans.woff2
subset "$cache/plex-sans-italic.ttf" ibm-plex-sans-italic.woff2

subset "$cache/IBMPlexMono-Regular.ttf" ibm-plex-mono-400.woff2
subset "$cache/IBMPlexMono-Medium.ttf" ibm-plex-mono-500.woff2
subset "$cache/IBMPlexMono-Italic.ttf" ibm-plex-mono-400-italic.woff2

cp "$cache/OFL-shippori.txt" "$out/OFL-shippori-mincho-b1.txt"
cp "$cache/OFL-plex-sans.txt" "$out/OFL-ibm-plex-sans.txt"
cp "$cache/OFL-plex-mono.txt" "$out/OFL-ibm-plex-mono.txt"
ls -l "$out"
