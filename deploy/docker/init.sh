#!/bin/sh
# Writes .env for one role, with generated secrets, so `docker compose up -d`
# works on its own. It never overwrites an existing .env.
set -eu
cd "$(dirname "$0")"

usage() {
	cat >&2 <<'USAGE'
usage:
  ./init.sh orchestrator PUBLIC_HOSTNAME CONTROL_HOSTNAME
  ./init.sh add-worker NAME             on the orchestrator node, prints the worker's command
  ./init.sh add-edge NAME URL QUOTA_GB  on the orchestrator node, prints the edge's command
  ./init.sh worker ORCHESTRATOR_URL TOKEN
  ./init.sh edge ORCHESTRATOR_URL TOKEN SIGNING_KEY EDGE_HOSTNAME
USAGE
	exit 2
}

secret() { head -c 48 /dev/urandom | base64 | tr -d '/+=\n'; }

fresh_env() {
	if [ -e .env ]; then
		echo "init: .env already exists, nothing changed" >&2
		exit 1
	fi
	umask 077
}

check_name() {
	case "$1" in
	"" | *[!a-z0-9-]*)
		echo "init: names use lowercase letters, digits and dashes" >&2
		exit 2
		;;
	esac
}

need_env() {
	if [ ! -e .env ]; then
		echo "init: run ./init.sh orchestrator first" >&2
		exit 1
	fi
}

# Reads a value from .env.
value() { sed -n "s/^$1=//p" .env; }

# Replaces a line in .env, keeping its mode.
set_value() {
	tmp=$(mktemp)
	awk -v key="$1" -v val="$2" 'index($0, key "=") == 1 { print key "=" val; next } { print }' .env >"$tmp"
	cat "$tmp" >.env
	rm -f "$tmp"
}

add_node() {
	if grep -q "^NODE_TOKENS=\(.*;\)\{0,1\}$1=" .env; then
		echo "init: node $1 already exists" >&2
		exit 1
	fi
	old=$(value NODE_TOKENS)
	set_value NODE_TOKENS "${old:+$old;}$1=$2"
}

[ $# -ge 1 ] || usage
role=$1
shift

case "$role" in
orchestrator)
	[ $# -eq 2 ] || usage
	fresh_env
	cat >.env <<ENV
COMPOSE_FILE=compose.orchestrator.yml
# Comma-separated: router, caddy, jellyfin. See compose.orchestrator.yml.
COMPOSE_PROFILES=

# auto or manual. Change it only with orchestrator switch-mode.
ENCODE_MODE=auto

# Your media, and one parent folder for the library and incoming uploads.
ORIGINALS_HOST=/srv/originals
STORE_HOST=/srv/store

JELLYFIN_URL=http://127.0.0.1:8096
# Create one in Jellyfin under Dashboard, API Keys.
JELLYFIN_TOKEN=
# Where Jellyfin sees STORE_HOST/library.
JELLYFIN_LIBRARY_DIR=/media

# Source data backfill may start per day. Size it from encode time on small workers.
BACKFILL_DAILY_GB=2

CONTROL_URL=https://$2
CONTROL_ADDR=127.0.0.1:8200
# The proxy on this machine. It names the real client for allowlists and rate limits.
TRUSTED_PROXIES=127.0.0.1
NODE_TOKENS=

ROUTER_ADDR=127.0.0.1:8204
EDGE_NODES=
EDGE_VIEWERS=
EDGE_TOKEN_KEY=$(secret)

#WEB_ADDR=127.0.0.1:8202
#NOTIFY_NTFY_URL=https://ntfy.sh/your-topic

# For the caddy profile.
PUBLIC_HOSTNAME=$1
PUBLIC_UPSTREAM=127.0.0.1:8096
CONTROL_HOSTNAME=$2
ENV
	echo "Wrote .env. Set JELLYFIN_TOKEN and your paths, then add nodes with ./init.sh add-worker NAME."
	;;
add-worker)
	[ $# -eq 1 ] || usage
	check_name "$1"
	need_env
	token=$(secret)
	add_node "$1" "$token"
	echo "Added $1. Restart with: docker compose up -d"
	echo "On the worker node, run:"
	echo "  ./init.sh worker $(value CONTROL_URL) $token"
	;;
add-edge)
	[ $# -eq 3 ] || usage
	check_name "$1"
	need_env
	token=$(secret)
	key=$(secret)
	add_node "$1" "$token"
	old=$(value EDGE_NODES)
	set_value EDGE_NODES "${old:+$old;}$1=$2,$3,$key"
	echo "Added $1. Restart with: docker compose up -d"
	echo "On the edge node, run:"
	echo "  ./init.sh edge $(value CONTROL_URL) $token $key ${2#*://}"
	;;
worker)
	[ $# -eq 2 ] || usage
	fresh_env
	cat >.env <<ENV
COMPOSE_FILE=compose.worker.yml
ORCHESTRATOR_URL=$1
NODE_TOKEN=$2
ENV
	echo "Wrote .env. Start with: docker compose up -d"
	;;
edge)
	[ $# -eq 4 ] || usage
	fresh_env
	cat >.env <<ENV
COMPOSE_FILE=compose.edge.yml
# Add caddy for automatic HTTPS if this node has no reverse proxy.
COMPOSE_PROFILES=
ORCHESTRATOR_URL=$1
NODE_TOKEN=$2
EDGE_SIGNING_KEY=$3
EDGE_HOSTNAME=$4
# Where your reverse proxy reaches edge-serve.
EDGE_BIND=127.0.0.1:8205
# Pull speed cap in MB per second, so a pull leaves room for viewers.
EDGE_LIMIT_MB=20
ENV
	echo "Wrote .env. Start with: docker compose up -d"
	;;
*)
	usage
	;;
esac
