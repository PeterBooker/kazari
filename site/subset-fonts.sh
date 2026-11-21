#!/usr/bin/env bash
# Builds site/static/fonts, and internal/theme/fonts for the pages the server renders itself (the
# dashboard, sign-in, account and gate pages). Rerun after adding Japanese text to site/, docs/ or a
# template or stylesheet under internal/: characters missing from the subset fall back to a system
# font. Needs curl and fonttools with brotli. "subset-fonts.sh site" or "subset-fonts.sh app" writes
# one of the two sets.
set -euo pipefail
cd "$(dirname "$0")"
only="${1:-all}"

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

# Every non-ASCII character the site uses; each subset keeps only those its font has.
latin="U+0020-007E,U+00A0-017F,U+0192,U+02C6,U+02DA,U+02DC,U+2000-206F,U+20AC,U+2122,U+2190-2199,U+2212,U+2215,U+25CF,U+FEFF,U+FFFD"
text="$cache/text.txt"
LC_ALL=C.UTF-8 grep -rhoP '[^\x{00}-\x{7F}]' layouts data content assets ../docs | sort -u | tr -d '\n' > "$text"

subset() { # source output unicodes [text-file] [out-dir]
  pyftsubset "$1" --unicodes="$3" --text-file="${4:-$text}" --layout-features='*' \
    --drop-tables+=meta --flavor=woff2 --output-file="${5:-$out}/$2"
}

licences() { # out-dir
  cp "$cache/OFL-manrope.txt" "$1/OFL-manrope.txt"
  cp "$cache/OFL-noto-sans-jp.txt" "$1/OFL-noto-sans-jp.txt"
  cp "$cache/OFL-dm-mono.txt" "$1/OFL-dm-mono.txt"
}

fonttools varLib.instancer -q "$cache/Manrope-VF.ttf" wght=300:700 -o "$cache/manrope.ttf"
fonttools varLib.instancer -q "$cache/NotoSansJP-VF.ttf" wght=200:700 -o "$cache/noto-sans-jp.ttf"

if [[ $only == all || $only == site ]]; then
  subset "$cache/manrope.ttf" manrope.woff2 "$latin"
  # Japanese only appears in [lang="ja"] elements, so no Latin beyond the space.
  subset "$cache/noto-sans-jp.ttf" noto-sans-jp.woff2 "U+0020,U+00A0"
  subset "$cache/DMMono-Regular.ttf" dm-mono-400.woff2 "$latin"
  subset "$cache/DMMono-Medium.ttf" dm-mono-500.woff2 "$latin"
  subset "$cache/DMMono-Italic.ttf" dm-mono-400-italic.woff2 "$latin"
  licences "$out"
  ls -l "$out"
fi

# The server's own pages: a smaller set, embedded in the binary. One mono weight and no italic; the
# Japanese is the few labels in the templates, the stylesheets and the Go files that fill them.
# japanese.txt lists what the subset holds, for the test that catches a label added without a rerun.
if [[ $only == all || $only == app ]]; then
  app=../internal/theme/fonts
  apptext="$cache/app-text.txt"
  mkdir -p "$app"
  LC_ALL=C.UTF-8 grep -rhoP '[^\x{00}-\x{7F}]' ../internal \
    --include='*.html' --include='*.css' --include='*.js' --include='*.go' --exclude='*_test.go' --exclude-dir=testdata \
    | sort -u | tr -d '\n' > "$apptext"
  subset "$cache/manrope.ttf" manrope.woff2 "$latin" "$apptext" "$app"
  subset "$cache/noto-sans-jp.ttf" noto-sans-jp.woff2 "U+0020,U+00A0" "$apptext" "$app"
  subset "$cache/DMMono-Regular.ttf" dm-mono-400.woff2 "$latin" "$apptext" "$app"
  LC_ALL=C.UTF-8 grep -oP '[\x{3000}-\x{30FF}\x{4E00}-\x{9FFF}]' "$apptext" | sort -u | tr -d '\n' > "$app/japanese.txt"
  licences "$app"
  ls -l "$app"
fi
