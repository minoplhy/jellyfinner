#!/bin/sh
# Read-only preflight for one node. It changes nothing.
#   deploy/check.sh orchestrator|worker|edge [PATH_TO_ENV]
set -u

role=${1:-}
env_file=${2:-$(dirname "$0")/docker/.env}
fails=0

pass() { printf 'PASS  %s\n' "$1"; }
fail() {
	printf 'FAIL  %s\n' "$1"
	fails=$((fails + 1))
}
value() { sed -n "s/^$1=//p" "$env_file" 2>/dev/null | tail -n 1; }
# Relative folders in .env are relative to the Compose files beside it.
host_path() {
	case "$1" in
	/*) printf '%s' "$1" ;;
	*) printf '%s/%s' "$(dirname "$env_file")" "$1" ;;
	esac
}

check_tool() {
	if command -v "$1" >/dev/null 2>&1; then pass "$1 installed"; else fail "$1 is not installed"; fi
}

check_dir_owner() {
	if [ ! -d "$1" ]; then
		fail "$1 does not exist"
	elif [ "$(stat -c %u "$1")" != "$2" ]; then
		fail "$1 must be owned by UID $2: run sudo ./init.sh dirs"
	else
		pass "$1 owned by UID $2"
	fi
}

check_free() {
	# -P gives the same columns on GNU and BusyBox, for Alpine nodes.
	free_kb=$(df -Pk "$1" 2>/dev/null | awk 'NR == 2 { print $4 }')
	free_gb=$((${free_kb:-0} / 1048576))
	if [ "$free_gb" -ge "$2" ]; then pass "$1 has $free_gb GB free"; else fail "$1 has $free_gb GB free, want at least $2"; fi
}

check_url() {
	if [ -z "$1" ]; then
		fail "no orchestrator URL in $env_file"
	# Any HTTP answer proves the route and TLS work. Without a token it is 401.
	elif curl -s -o /dev/null --max-time 10 "$1/v1/jobs/claim" -X POST; then
		pass "reached $1"
	else
		fail "cannot reach $1"
	fi
}

check_set() {
	if [ -n "$(value "$1")" ]; then pass "$1 is set"; else fail "$1 is empty in $env_file"; fi
}

if [ "$role" != orchestrator ] && [ "$role" != worker ] && [ "$role" != edge ]; then
	echo "usage: check.sh orchestrator|worker|edge [PATH_TO_ENV]" >&2
	exit 2
fi

if [ -f "$env_file" ]; then pass "found $env_file"; else fail "$env_file is missing, run init.sh"; fi
check_tool curl
if docker compose version >/dev/null 2>&1 || podman compose version >/dev/null 2>&1; then
	pass "compose available"
else
	fail "docker compose is not available"
fi

case "$role" in
orchestrator)
	originals=$(value ORIGINALS_HOST)
	store=$(value STORE_HOST)
	state_dir=$(value STATE_HOST)
	check_set NODE_TOKENS
	check_set JELLYFIN_TOKEN
	if [ -d "${originals:-/nonexistent}" ]; then pass "$originals exists"; else fail "originals folder ${originals:-unset} does not exist"; fi
	check_dir_owner "$(host_path "${state_dir:-data/state}")" 10001
	check_dir_owner "${store:-/nonexistent}" 10001
	check_dir_owner "${store:-/nonexistent}/library" 10001
	check_dir_owner "${store:-/nonexistent}/incoming" 10001
	# Compose mounts the parent, so these only need to sit on one filesystem.
	library_dev=$(stat -c %d "${store:-/nonexistent}/library" 2>/dev/null)
	incoming_dev=$(stat -c %d "${store:-/nonexistent}/incoming" 2>/dev/null)
	if [ -n "$library_dev" ] && [ "$library_dev" = "$incoming_dev" ]; then
		pass "library and incoming share a filesystem"
	else
		fail "library and incoming must be on one filesystem"
	fi
	check_free "${store:-/}" 50
	jellyfin=$(value JELLYFIN_URL)
	if curl -s -o /dev/null --max-time 5 "${jellyfin:-http://127.0.0.1:8096}/System/Info/Public"; then
		pass "reached Jellyfin"
	else
		fail "cannot reach Jellyfin at ${jellyfin:-unset}"
	fi
	;;
worker)
	check_url "$(value ORCHESTRATOR_URL)"
	check_set NODE_TOKEN
	scratch=$(host_path "$(value SCRATCH_HOST)")
	check_dir_owner "$scratch" 10001
	check_free "$scratch" 50
	;;
edge)
	check_url "$(value ORCHESTRATOR_URL)"
	check_set NODE_TOKEN
	check_set EDGE_SIGNING_KEY
	check_set EDGE_HOSTNAME
	edge_dir=$(host_path "$(value EDGE_HOST)")
	check_dir_owner "$edge_dir" 10001
	check_free "$edge_dir" 20
	;;
esac

if [ "$fails" -gt 0 ]; then
	echo "$fails check(s) failed"
	exit 1
fi
echo "All checks passed"
