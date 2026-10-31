#!/bin/sh
# Proves the compose files work as an operator would use them: edited copies in
# an empty folder, with no repository, script or .env beside them. It encodes a
# short generated clip end to end. Needs docker compose, ffmpeg and sudo-free
# Docker. Usage: smoke.sh [IMAGE_TAG]
set -eu

tag=${1:-latest}
here=$(cd "$(dirname "$0")" && pwd)
root=$(mktemp -d)
cleanup() {
	(cd "$root/worker" && docker compose logs --tail 20 || true)
	(cd "$root/orchestrator" && docker compose logs --tail 20 || true)
	(cd "$root/worker" && docker compose down -t 2 || true)
	(cd "$root/orchestrator" && docker compose down -t 2 || true)
}
trap cleanup EXIT

mkdir -p "$root/orchestrator" "$root/worker" "$root/media/Show"
ffmpeg -loglevel error -f lavfi -i testsrc2=size=320x180:rate=24:duration=4 \
	-f lavfi -i sine=frequency=440:duration=4 -pix_fmt yuv420p -c:a aac "$root/media/Show/Episode 01.mkv"

sed -e "s#:latest#:$tag#" \
	-e "s#/srv/originals:/originals#$root/media:/originals#" \
	-e 's#/srv/store:/store#./data/store:/store#' \
	-e 's#https://control.example.com#http://orchestrator:8200#' \
	"$here/compose.orchestrator.yml" >"$root/orchestrator/compose.yml"

cd "$root/orchestrator"
# Not up --wait, which counts the *-init services exiting as a failure.
docker compose up -d
run() { docker compose exec -T orchestrator orchestrator "$@"; }
for _ in $(seq 1 30); do
	run health serve 2>/dev/null && break
	sleep 2
done
token=$(run node add worker1 | sed -n 's/^ *NODE_TOKEN: //p')

sed -e "s#:latest#:$tag#" \
	-e 's#https://control.example.com#http://orchestrator:8200#' \
	-e "s#replace-with-the-printed-token#$token#" \
	-e 's/^    # networks: \[jellyfinner\]/    networks: [jellyfinner]/' \
	-e 's/^# networks:/networks:/' -e 's/^#   jellyfinner:/  jellyfinner:/' -e 's/^#     external: true/    external: true/' \
	"$here/compose.worker.yml" >"$root/worker/compose.yml"

(cd "$root/worker" && docker compose up -d)
(cd "$root/worker" && docker compose run --rm -T worker doctor | grep 'token accepted' | grep PASS)

run queue add "Show/Episode 01.mkv"
for _ in $(seq 1 60); do
	if run queue list | grep -q ' done '; then
		docker compose exec -T orchestrator test -s "/store/library/Show/Episode 01.mp4"
		echo "smoke test passed"
		exit 0
	fi
	sleep 5
done
echo "the job did not finish" >&2
run queue list >&2
exit 1
