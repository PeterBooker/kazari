#!/bin/sh
# Runs before the deb or rpm package is removed; an upgrade keeps the
# service running ($1 is "upgrade" on deb and 1 on rpm).
set -e

case "$1" in
remove | 0)
	if command -v systemctl >/dev/null 2>&1; then
		systemctl disable --now kazari >/dev/null 2>&1 || true
	fi
	;;
esac
