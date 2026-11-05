#!/bin/sh
# Jellyfinner worker, started by the orchestrator, which also deletes this
# server once it is idle. Do not keep anything here.
set -eu
if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
	export DEBIAN_FRONTEND=noninteractive
	apt-get update
	apt-get install -y docker.io docker-compose-v2
fi
mkdir -p /opt/jellyfinner
cd /opt/jellyfinner
cat >compose.yml <<'JELLYFINNER_COMPOSE'
# Mirrors deploy/docker/compose.worker.yml, filled in for this server.
name: jellyfinner-worker

x-owner: &owner "10001:10001"

x-image: &image {{.Image}}

services:
  worker-init:
    image: *image
    user: "0:0"
    command: ["prepare", "--owner", *owner, "/scratch:750"]
    volumes: &volumes
      - ./data/scratch:/scratch
    cap_drop:
      - ALL
    cap_add: [CHOWN, DAC_OVERRIDE, FOWNER]
    security_opt:
      - no-new-privileges:true

  worker:
    image: *image
    user: *owner
    restart: unless-stopped
    environment:
      ORCHESTRATOR_URL: {{.URL}}
      NODE_TOKEN: {{.Token}}
    volumes: *volumes
    read_only: true
    tmpfs:
      - /tmp
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    healthcheck:
      test: ["CMD", "worker", "health"]
      interval: 30s
      timeout: 10s
      start_period: 30s
JELLYFINNER_COMPOSE
docker compose up -d
