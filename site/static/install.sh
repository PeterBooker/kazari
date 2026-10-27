#!/bin/sh
# Installs the kazari binary from a GitHub release, verified against the
# release's checksums.txt. Usage:
#
#   curl -fsSL https://kazari.io/install.sh | sh
#
#   KAZARI_VERSION=0.3.0 ...   installs that release instead of the latest
#   KAZARI_BINDIR=~/bin ...    installs there instead of /usr/local/bin
#
# It installs only the binary. The deb and rpm packages on the release
# page add the systemd unit and the configuration files; the guide at
# https://kazari.io/docs/install/ has the manual steps.
set -eu

main() {
	repo="PeterBooker/kazari"
	version="${KAZARI_VERSION:-}"
	bindir="${KAZARI_BINDIR:-/usr/local/bin}"

	for tool in curl tar; do
		command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
	done

	case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) fail "unsupported operating system $(uname -s): releases are built for Linux and macOS" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "unsupported architecture $(uname -m): releases are built for amd64 and arm64" ;;
	esac
	if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
		fail "releases are built for macOS on Apple silicon only"
	fi

	if [ -z "$version" ]; then
		version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" |
			sed -n 's/^ *"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
		[ -n "$version" ] || fail "could not find the latest release; set KAZARI_VERSION"
	fi

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	base="https://github.com/$repo/releases/download/$version"
	archive="kazari_${version}_${os}_${arch}.tar.gz"
	echo "Downloading kazari $version for $os/$arch"
	curl -fsSL -o "$tmp/$archive" "$base/$archive" || fail "no release $version for $os/$arch at $base/$archive"
	curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "no checksums.txt for release $version"

	(
		cd "$tmp"
		grep " $archive\$" checksums.txt >expected.txt || fail "$archive is not in checksums.txt"
		if command -v sha256sum >/dev/null 2>&1; then
			sha256sum -c expected.txt >/dev/null
		else
			shasum -a 256 -c expected.txt >/dev/null
		fi
	) || fail "checksum of $archive does not match checksums.txt"
	tar -xzf "$tmp/$archive" -C "$tmp" kazari

	if [ -w "$bindir" ] || { [ ! -e "$bindir" ] && mkdir -p "$bindir" 2>/dev/null; }; then
		install -m 0755 "$tmp/kazari" "$bindir/kazari"
	else
		echo "Installing to $bindir needs sudo"
		sudo install -m 0755 "$tmp/kazari" "$bindir/kazari"
	fi

	echo "Installed $("$bindir/kazari" version) to $bindir/kazari"
	echo
	echo "Next, on a server: put the configuration in /etc/kazari (https://kazari.io/docs/install/),"
	echo "then run: sudo kazari check"
	echo "On a laptop, to try it: kazari serve --local"
}

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

main "$@"
