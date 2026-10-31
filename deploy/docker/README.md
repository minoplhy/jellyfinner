# Running Jellyfinner with Docker Compose

For operators deploying with Docker Compose, which is the main way to run Jellyfinner. For a full first deployment with a check at every stage, follow [../first-run.md](../first-run.md).

There are three roles. Any role can run on any machine in any region, and several can share one machine. Give each role on a machine its own copy of this folder, because each copy has its own `.env`.

| Role | Runs | Compose file |
|---|---|---|
| Orchestrator node | Orchestrator, control API, admin UI, optional router, Caddy and Jellyfin | `compose.orchestrator.yml` |
| Worker node | Encoder | `compose.worker.yml` |
| Edge node | File server and pull agent | `compose.edge.yml` |

## Images

Build once on any capable machine, from the repository root, then move the images to your nodes. This saves building on small servers.

```sh
docker build -f deploy/docker/Dockerfile.orchestrator -t jellyfinner/orchestrator:local .
docker build -f deploy/docker/Dockerfile.worker -t jellyfinner/worker:local .
docker build -f deploy/docker/Dockerfile.edge-agent -t jellyfinner/edge-agent:local .

docker save jellyfinner/worker:local | ssh worker-host docker load
```

If you push to a registry, set `JELLYFINNER_IMAGE_PREFIX` and `JELLYFINNER_TAG` in `.env`.

On GitHub, `.github/workflows/images.yml` does this for you. Every push to the default branch or a `v*` tag runs the tests, then pushes `linux/amd64` images to GitHub Container Registry. Pull requests only build. To use those images:

```sh
JELLYFINNER_IMAGE_PREFIX=ghcr.io/your-account/your-repo
JELLYFINNER_TAG=latest
```

Then run `docker compose pull` before `docker compose up -d`. Images in a private repository also need `docker login ghcr.io` on each node.

## Setup

`init.sh` writes `.env`, including generated secrets, and never overwrites an existing one. It also sets `COMPOSE_FILE`, so `docker compose up -d` needs no flags.

On the orchestrator node:

```sh
./init.sh orchestrator tv.example.com control.example.com
./init.sh add-worker worker1
./init.sh add-edge edge1 https://edge1.example.com 500
```

Set `JELLYFIN_TOKEN`, `ORIGINALS_HOST` and `STORE_HOST` in `.env`. The container runs as UID 10001, so:

```sh
sudo mkdir -p /srv/store/library /srv/store/incoming
sudo chown -R 10001:10001 /srv/store
docker compose up -d
```

Library and incoming must sit under one mount, because a rename cannot cross two mounts. The orchestrator refuses to start if it would fail.

The `add-worker` and `add-edge` commands each print one command. Run it on that node, then start it:

```sh
./init.sh worker https://control.example.com GENERATED_TOKEN
docker compose up -d
```

Run `docker compose up -d` on the orchestrator after adding nodes, so it reads the new tokens.

Run CLI commands through the container:

```sh
docker compose exec orchestrator orchestrator --env /nonexistent queue list
```

## Optional parts

Set these in `COMPOSE_PROFILES` in `.env`, separated by commas.

| Profile | Where | Gives you |
|---|---|---|
| `caddy` | Orchestrator, edge | Automatic HTTPS, if you have no reverse proxy. It needs ports 80 and 443 |
| `router` | Orchestrator | Redirects cached titles to edge nodes |
| `jellyfin` | Orchestrator | Runs Jellyfin, if you do not have one already |

## Your own reverse proxy

Nothing requires a particular proxy. On an edge node, point yours at `EDGE_BIND`. See [../proxy/README.md](../proxy/README.md).

On the orchestrator node, send the viewer hostname to Jellyfin, or to `ROUTER_ADDR` once the router is proven, and the control hostname to `CONTROL_ADDR`. List the proxy's address in `TRUSTED_PROXIES`, so IP allowlists and failed-login limits see real clients. Do not expose `CONTROL_ADDR` directly while `TRUSTED_PROXIES` is set for an address outsiders can reach.

## Worker options

Add these with `COMPOSE_FILE` in `.env`, separated by a colon:

```
COMPOSE_FILE=compose.worker.yml:compose.worker-gpu.yml
```

| File | Purpose |
|---|---|
| `compose.worker-gpu.yml` | Encodes with a VAAPI GPU. It needs `RENDER_GID`, from `getent group render \| cut -d: -f3` |
| `compose.worker-rclone.yml` | Transfers with rclone over SFTP, for the speed test in [../first-run.md](../first-run.md) |

## Status and logs

```sh
docker compose ps
docker compose logs -f
```

Every service except Caddy and Jellyfin has a health check. The orchestrator and router check their port, `edge-serve` checks its `/health`, and the worker and pull agent report unhealthy if they have not reached the orchestrator for three minutes.

## Security settings

Our services run as non-root users, on a read-only root filesystem, with all capabilities dropped. On an edge node, the pull agent holds only the node token and can write files. The file server holds only the signing key and reads files, with a 64 MB memory limit against the agent's 128 MB. Read [../edge-security.md](../edge-security.md) before enrolling a real node.
