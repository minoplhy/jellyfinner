# Running Jellyfinner with Docker Compose

For operators deploying with Docker Compose, which is the main way to run Jellyfinner. For a full first deployment with a check at every stage, follow [../first-run.md](../first-run.md).

Each role is one self-contained compose file. Any role can run on any machine in any region, and several can share one machine.

| Role | Runs | Compose file |
|---|---|---|
| Orchestrator node | Orchestrator, control API, admin UI. Optional router, Caddy and Jellyfin | [compose.orchestrator.yml](compose.orchestrator.yml) |
| Worker node | Encoder | [compose.worker.yml](compose.worker.yml) |
| Edge node | File server and pull agent | [compose.edge.yml](compose.edge.yml) |

Use a file as it is, or paste its services into your own `docker-compose.yaml`, Portainer stack or similar. It needs no script, no `.env`, and no other file. Values you must change are marked `EDIT`. If you prefer a `.env`, replace any value with `${VAR}` as usual.

**Keep a filled-in file private and out of git,** because it holds node secrets.

## Setup

### Orchestrator node

Paste `compose.orchestrator.yml`, edit the marked values, then start it and check it:

```sh
docker compose up -d
docker compose run --rm orchestrator doctor
```

It starts with no nodes. Point your reverse proxy at its ports, or uncomment the `caddy` block.

### Add a worker

On the orchestrator node:

```sh
docker compose exec orchestrator orchestrator node add worker1
```

It prints `ORCHESTRATOR_URL` and `NODE_TOKEN`. Paste `compose.worker.yml` on the worker, fill those in, then:

```sh
docker compose up -d
docker compose run --rm worker doctor
```

The token is shown once, and the database keeps only its hash. If you lose it, run `node rotate worker1`.

### Add an edge node

```sh
docker compose exec orchestrator orchestrator node add edge1 --edge --url https://edge1.example.com --quota-gb 500
```

It prints two blocks, so `edge-pull` gets only the token and `edge-serve` gets only the signing key. Keep them apart. Read [../edge-security.md](../edge-security.md) before enrolling a real node.

```sh
docker compose up -d
docker compose run --rm edge-pull doctor
docker compose run --rm edge-serve doctor
```

### Manage nodes

Changes reach running services within about 10 seconds, with no restart.

```sh
docker compose exec orchestrator orchestrator node list
docker compose exec orchestrator orchestrator node rotate worker1
docker compose exec orchestrator orchestrator node remove worker1
```

`node remove` revokes the node and forgets what it cached. Files stay on an edge node's disk until you delete them.

To limit a node to known addresses, add it with `--allow 203.0.113.7/32`.

## Folders and ownership

All data lives in plain folders, not Docker volumes. A short-lived `*-init` service creates them with the right owner, then exits, so no `sudo` or host command is needed. It changes only the folders themselves, never what is inside them, and never your originals.

| Role | Folder in the container | Holds |
|---|---|---|
| Orchestrator | `/var/lib/jellyfinner` | The database, including node secrets. Back this up and keep it private |
| Orchestrator | `/store/library`, `/store/incoming` | Published encodes and uploads in progress |
| Worker | `/scratch` | Sources and encodes in progress |
| Edge | `/edge` | Cached files |

Library and incoming must sit under one mount, because a rename cannot cross two mounts. The orchestrator refuses to start if it would fail.

Do not add `depends_on` from a service to its `*-init` service. On Podman, that stops a crashed service restarting. Services wait up to 30 seconds for their folders instead.

**If your media belongs to another user,** set `x-owner` at the top of the file to that `UID:GID`. It sets both the services' user and the folders' owner.

**On rootless Docker or Podman,** files you own appear to container UID 10001 as someone else's. World-readable originals work as they are. For private ones, set `x-owner` to `"0:0"`, which is your own host user there and not real root.

**On a network share** where `chown` is refused, set the owner on the server. `*-init` changes nothing that is already right.

## Networking

Everything runs on a normal bridge network named `jellyfinner`, and ports are published on `127.0.0.1` only, for a reverse proxy on the same machine.

| Port | Purpose |
|---|---|
| `8200` | Control API, for workers and edge nodes |
| `8202` | Admin UI |
| `8201` | Playback events from Jellyfin, in manual mode. Commented out by default |
| `8204` | Router, if enabled |
| `8205` | `edge-serve`, on an edge node |

Nodes need no open ports, except an edge node's HTTPS. They use HTTPS, or plain HTTP inside a private network: a private address, a one-word container name such as `orchestrator`, or a `.internal` name.

**Jellyfin on the host** is reached at `http://host.docker.internal:8096`, so it must listen on more than `127.0.0.1`. **Jellyfin in the same file** is `http://jellyfin:8096`.

**A worker on the orchestrator's machine** joins the network: uncomment `networks` in `compose.worker.yml`, and use `http://orchestrator:8200`.

**A reverse proxy in another compose project** joins by network instead of by port. Add its network to the end of the file with `external: true`, list it under the service's `networks`, then proxy to `orchestrator:8200`.

### Reverse proxies

Nothing requires a particular proxy. On an edge node, see [../proxy/README.md](../proxy/README.md). On the orchestrator node, send the viewer hostname to Jellyfin, or to the router once it is proven, and the control hostname to port `8200`.

### Serving under a path

Subdomains need nothing extra, and paths such as `https://example.com/jellyfinner` work too. Each part answers both with and without its prefix, so your proxy may strip the prefix or keep it.

| Part | What to set |
|---|---|
| Control API | Put the path in `CONTROL_URL`, such as `https://example.com/jellyfinner`. `node add` prints it for nodes |
| Admin UI | `WEB_BASE_PATH`, such as `/jellyfinner-admin` |
| Jellyfin and the router | Set Jellyfin's Base URL under Dashboard, Networking, and include it in `JELLYFIN_URL`, such as `http://host.docker.internal:8096/jellyfin`. Proxy that path to the router or to Jellyfin |
| Edge node | Add it with `--url https://example.com/edge1`. `edge-serve` needs no setting |

HTTP/2 on your proxy is fine, such as nginx with `http2 on;`. File transfers use HTTP/1.1 on purpose, so parallel streams get their own connections, and every other request may use HTTP/2.

The control API and the admin UI need different paths, because each is its own listener.

The image trusts `X-Forwarded-For` from private addresses, so allowlists and failed-login limits see real clients behind a proxy. This is safe while ports stay on `127.0.0.1`. **If you publish the control API on a public address, set `TRUSTED_PROXIES` to your proxy's address, or to an empty value.** Otherwise, on a runtime that hides client addresses, an outsider could forge the header to get around those limits.

## Optional parts

Uncomment these blocks in the file.

| Block | Where | Gives you |
|---|---|---|
| `caddy` | Orchestrator, edge | Automatic HTTPS, if you have no reverse proxy. It needs ports 80 and 443 |
| `router` | Orchestrator | Redirects cached titles to edge nodes. Needs `JELLYFIN_TOKEN` |
| `jellyfin` | Orchestrator | Runs Jellyfin, if you do not have one already |
| `ENCODER`, `devices`, `group_add` | Worker | Encodes with a VAAPI GPU, or with RKMPP on a Rockchip board running its vendor kernel. The worker image is built for amd64 and arm64 |
| `TRANSFER`, the `rclone` volume | Worker | Transfers with rclone over SFTP, for the speed test in [../first-run.md](../first-run.md) |

## Manual mode

Manual mode encodes a title when someone presses play on its stub, so Jellyfin must be able to tell the orchestrator. Set these in `compose.orchestrator.yml` before the first start:

- `ENCODE_MODE: manual`
- `JELLYFIN_TOKEN`, which manual mode requires
- `WEBHOOK_TOKEN`, at least 32 characters, such as the output of `openssl rand -hex 32`

Then install the Webhook plugin in Jellyfin and add a Generic destination:

| Setting | Value |
|---|---|
| Webhook URL | `http://orchestrator:8201/webhook/jellyfin` for Jellyfin on the `jellyfinner` network. For Jellyfin on the host, uncomment port `8201` and use `http://127.0.0.1:8201/webhook/jellyfin` |
| Notification type | Playback Start |
| Template | The contents of [../jellyfin-webhook.json](../jellyfin-webhook.json) |
| Header | `X-Webhook-Token`, with your `WEBHOOK_TOKEN` |

Keep the port on `127.0.0.1` or a private address. To change the mode of a running deployment, use `orchestrator switch-mode`, with `serve` stopped.

## Images and updates

Compose pulls prebuilt `linux/amd64` images from GitHub Container Registry, so nodes build nothing. Every push to the default branch and every `v*` tag publishes new images.

```sh
docker compose pull
docker compose up -d
```

`latest` changes with every push, so nodes updated at different times can run different versions. To keep every node matched, use the same tag in each file's `image:` lines, such as `1.2.3` or `sha-c4f5714`.

To build from source, use the matching `.dev.yaml` on top, which needs the whole repository:

```sh
docker compose -f compose.worker.yml -f compose.worker.dev.yaml up -d --build
```

## Everyday commands

```sh
docker compose exec orchestrator orchestrator queue list
docker compose ps
docker compose logs -f
```

`doctor` runs in its own short-lived container, so it works while a service is crash-looping, and sees the same mounts, user and network. Fix whatever it reports as `FAIL`. Every service except Caddy and Jellyfin also has a health check. The worker and pull agent report unhealthy if they have not reached the orchestrator for three minutes.

## Security settings

Our services run as non-root users, on a read-only root filesystem, with all capabilities dropped. Only the `*-init` services run as root, with the few capabilities needed to set ownership, and they exit at once. On an edge node, the pull agent holds only the node token and can write files. The file server holds only the signing key and reads files, with a 64 MB memory limit against the agent's 128 MB.
