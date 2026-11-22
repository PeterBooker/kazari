#!/bin/sh
# Runs after the deb or rpm package is installed or upgraded.
set -e

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
	# A running instance picks up the new binary on restart.
	if systemctl is-active --quiet kazari 2>/dev/null; then
		systemctl restart kazari || true
		exit 0
	fi
fi

cat <<'MSG'
Kazari is installed. To start it:
  1. Edit /etc/kazari/kazari.yaml (your domain and DNS provider)
     and /etc/kazari/kazari.env (the DNS provider's token).
  2. sudo kazari check     (reads both files)
  3. sudo systemctl enable --now kazari
  4. sudo kazari admin setup-link
     and open the link it prints to create the first admin.
Guide: https://kazari.io/docs/install/
MSG
