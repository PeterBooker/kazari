#!/usr/bin/env bash
# Builds the self-hosted web fonts in site/static/fonts from the google/fonts repository at one commit:
#   Manrope 300-700                 headings and body; one variable file, Latin
#   Noto Sans JP 200-700            Japanese; one variable file, only the characters the site uses
#   DM Mono 400, 500, italic        code, hostnames and small labels; Latin
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
fetch manrope 'Manrope%5Bwght%5D.ttf' Manrope-VF.ttf
fetch manrope OFL.txt OFL-manrope.txt
fetch notosansjp 'NotoSansJP%5Bwght%5D.ttf' NotoSansJP-VF.ttf
fetch notosansjp OFL.txt OFL-noto-sans-jp.txt
fetch dmmono DMMono-Regular.ttf
fetch dmmono DMMono-Medium.ttf
fetch dmmono DMMono-Italic.ttf
fetch dmmono OFL.txt OFL-dm-mono.txt

# Latin, punctuation and arrows for the Latin faces, plus each character outside ASCII that the site
# and docs use; a subset keeps only the characters its font has.
latin="U+0020-007E,U+00A0-017F,U+0192,U+02C6,U+02DA,U+02DC,U+2000-206F,U+20AC,U+2122,U+2190-2199,U+2212,U+2215,U+25CF,U+FEFF,U+FFFD"
text="$cache/text.txt"
LC_ALL=C.UTF-8 grep -rhoP '[^\x{00}-\x{7F}]' layouts data content assets ../docs | sort -u | tr -d '\n' > "$text"

subset() { # source output unicodes
  pyftsubset "$1" --unicodes="$3" --text-file="$text" --layout-features='*' \
    --drop-tables+=meta --flavor=woff2 --output-file="$out/$2"
}

# The variable faces keep only the weights the site sets.
fonttools varLib.instancer -q "$cache/Manrope-VF.ttf" wght=300:700 -o "$cache/manrope.ttf"
fonttools varLib.instancer -q "$cache/NotoSansJP-VF.ttf" wght=200:700 -o "$cache/noto-sans-jp.ttf"
subset "$cache/manrope.ttf" manrope.woff2 "$latin"
# Japanese text is only ever in [lang="ja"] elements, so this face needs no Latin beyond the space.
subset "$cache/noto-sans-jp.ttf" noto-sans-jp.woff2 "U+0020,U+00A0"

subset "$cache/DMMono-Regular.ttf" dm-mono-400.woff2 "$latin"
subset "$cache/DMMono-Medium.ttf" dm-mono-500.woff2 "$latin"
subset "$cache/DMMono-Italic.ttf" dm-mono-400-italic.woff2 "$latin"

cp "$cache/OFL-manrope.txt" "$out/OFL-manrope.txt"
cp "$cache/OFL-noto-sans-jp.txt" "$out/OFL-noto-sans-jp.txt"
cp "$cache/OFL-dm-mono.txt" "$out/OFL-dm-mono.txt"
ls -l "$out"
