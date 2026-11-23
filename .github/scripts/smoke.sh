#!/bin/sh
# Smoke test of a built binary: starts it with serve --local in an empty
# directory, publishes a page with its own CLI, fetches it, pulls it and
# deploys the pulled directory again.
#
#   .github/scripts/smoke.sh ./kazari
set -eu

kazari=$(realpath "$1")
port=${KAZARI_SMOKE_PORT:-18080}
work=$(mktemp -d)
cd "$work"

export KAZARI_LISTEN_HTTP="127.0.0.1:$port" KAZARI_EXTERNAL_PORT="$port"
"$kazari" serve --local >server.log 2>&1 &
server=$!
trap 'kill "$server" 2>/dev/null || true; rm -rf "$work"' EXIT

tries=0
until curl -fsS -o /dev/null "http://kazari.localhost:$port/health" 2>/dev/null; do
	tries=$((tries + 1))
	if [ "$tries" -ge 100 ] || ! kill -0 "$server" 2>/dev/null; then
		echo "the server did not answer /health:" >&2
		cat server.log >&2
		exit 1
	fi
	sleep 0.1
done

"$kazari" admin health
"$kazari" admin users create smoke --role admin
key=$("$kazari" admin keys issue smoke --label smoke --json | jq -r .key)

mkdir page
echo '<!doctype html><title>Smoke</title><p>kazari smoke test</p>' >page/index.html
KAZARI_URL="http://api.kazari.localhost:$port" KAZARI_TOKEN="$key" \
	"$kazari" publish page --slug smoke --visibility public

if ! curl -fsS "http://smoke.kazari.localhost:$port/" | grep -q 'kazari smoke test'; then
	echo "the published page was not served:" >&2
	cat server.log >&2
	exit 1
fi

KAZARI_URL="http://api.kazari.localhost:$port" KAZARI_TOKEN="$key" \
	"$kazari" pull smoke pulled
diff -r page pulled
echo '<!doctype html><title>Smoke</title><p>kazari smoke test, pulled</p>' >pulled/index.html
KAZARI_URL="http://api.kazari.localhost:$port" KAZARI_TOKEN="$key" \
	"$kazari" deploy pulled --slug smoke
if ! curl -fsS "http://smoke.kazari.localhost:$port/" | grep -q 'kazari smoke test, pulled'; then
	echo "the pulled and deployed page was not served:" >&2
	cat server.log >&2
	exit 1
fi
echo "smoke test passed"
