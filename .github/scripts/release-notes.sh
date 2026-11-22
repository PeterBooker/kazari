#!/bin/sh
# Prints the section of CHANGELOG.md for a version, without its heading: the
# release notes of that tag. Fails when the section is missing or empty, so a
# tag cannot be released before its changelog entry is merged.
#
#   .github/scripts/release-notes.sh VERSION [CHANGELOG]
set -eu

version=${1:?usage: release-notes.sh VERSION [CHANGELOG]}
changelog=${2:-CHANGELOG.md}

notes=$(awk -v heading="## $version" '
	$0 == heading { found = 1; next }
	found && /^## / { exit }
	found { print }
' "$changelog" | sed '/./,$!d')

if [ -z "$notes" ]; then
	echo "release-notes.sh: $changelog has no section \"## $version\"" >&2
	exit 1
fi
printf '%s\n' "$notes"
