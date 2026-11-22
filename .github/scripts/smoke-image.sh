#!/bin/sh
# Smoke test of a container image: starts it with serve --local on a new
# volume and waits for the health check over the admin socket, which the
# server can only create when the volume is writable by the image's user.
#
#   .github/scripts/smoke-image.sh kazari:dev
set -eu

image=$1
port=${KAZARI_SMOKE_PORT:-18080}
name="kazari-smoke-$$"

docker run --detach --name "$name" --publish "127.0.0.1:$port:8080" \
	--env KAZARI_LISTEN_HTTP=:8080 --env KAZARI_EXTERNAL_PORT="$port" \
	"$image" serve --local >/dev/null
trap 'docker rm --force --volumes "$name" >/dev/null' EXIT

tries=0
until docker exec "$name" kazari admin health 2>/dev/null; do
	tries=$((tries + 1))
	if [ "$tries" -ge 100 ] || [ "$(docker inspect --format '{{.State.Running}}' "$name")" != true ]; then
		echo "the container did not become healthy:" >&2
		docker logs "$name" >&2
		exit 1
	fi
	sleep 0.1
done

curl -fsS "http://kazari.localhost:$port/health"
echo
echo "image smoke test passed"
