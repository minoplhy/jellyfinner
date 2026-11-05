# Running Jellyfinner with Docker Compose

For operators deploying with Docker Compose, which is the main way to run Jellyfinner. For a full first deployment with a check at every stage, follow [../first-run.md](../first-run.md).

Each role is one self-contained compose file. Any role can run on any machine in any region, and several can share one machine.

| Role | Runs | Compose file |
|---|---|---|
| Orchestrator node | Orchestrator, control API, admin UI. Optional Caddy and Jellyfin | [compose.orchestrator.yml](compose.orchestrator.yml) |
| Worker node | Encoder | [compose.worker.yml](compose.worker.yml) |
| Edge node | File server and pull agent | [compose.edge.yml](compose.edge.yml) |
| Router | Proxy in front of Jellyfin that sends viewers to their edge node. Runs next to Jellyfin | [compose.router.yml](compose.router.yml) |

Use a file as it is, or paste its services into your own `docker-compose.yaml`, Portainer stack or similar. It needs no script, no `.env`, and no other file. Values you must change are marked `EDIT`. If you prefer a `.env`, replace any value with `${VAR}` as usual.

**Keep a filled-in file private and out of git,** because it holds node secrets.

**Upgrading:** the admin UI settings were renamed from `WEB_*` to `ADMIN_*`. `WEB_ADDR` is now `ADMIN_ADDR`, `WEB_TLS_CERT` and `WEB_TLS_KEY` are now `ADMIN_TLS_CERT` and `ADMIN_TLS_KEY`, and `WEB_BASE_PATH` is replaced by `ADMIN_URL`, the full address. The orchestrator refuses to start while an old name is set, and the error names the new setting.

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

It prints `ORCHESTRATOR_URL` and `NODE_TOKEN`. The admin UI's Nodes page does the same with "Add an encoding node". Paste `compose.worker.yml` on the worker, fill those in, then:

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

### Add a router

Viewers reach edge nodes through a router: a proxy that runs on the Jellyfin server, in front of Jellyfin. Every request passes straight through to Jellyfin, except a direct stream of a title cached on that viewer's edge node, which gets a redirect there. The router asks the orchestrator where each viewer goes, so it holds no edge secret, and if it cannot reach the orchestrator everyone simply streams from Jellyfin.

```sh
docker compose exec orchestrator orchestrator node add jf-router --router
```

On the Jellyfin server, fill in [compose.router.yml](compose.router.yml) with the printed token, plus `JELLYFIN_URL`, `JELLYFIN_TOKEN` and `JELLYFIN_LIBRARY_DIR`, the library folder as Jellyfin sees it. Then:

```sh
docker compose up -d
docker compose run --rm router doctor
```

Test against port `8204` as in [../first-run.md](../first-run.md) step 6. Once it works, point your public Jellyfin hostname at the router instead of at Jellyfin, and add the router's address to Jellyfin's Known proxies, under Dashboard, Networking, so Jellyfin still sees viewers' real addresses. Assign viewers on each edge node's Edit page.

**Upgrading:** `orchestrator router` and the `router` block in the orchestrator file are gone. Add a router node as above and run `compose.router.yml` instead, on the Jellyfin server or beside the orchestrator if Jellyfin runs there.

### Manage nodes

Changes reach running services within about 10 seconds, with no restart.

```sh
docker compose exec orchestrator orchestrator node list
docker compose exec orchestrator orchestrator node edit worker1 --name gpu-box --allow 10.8.0.0/24
docker compose exec orchestrator orchestrator node edit edge1 --url https://example.com/edge1 --quota-gb 200
docker compose exec orchestrator orchestrator node rotate worker1
docker compose exec orchestrator orchestrator node remove worker1
```

`node edit` changes only what you give it, and the Nodes page has an Edit button for the same. The node keeps its token and signing key, so nothing changes in its compose file:

- **Name:** jobs, usage history and cached titles follow the node through a rename, because they refer to its ID, shown by `node list`. So do an edge node's viewers.
- **Allowed addresses:** `--allow ""` accepts any address.
- **Edge URL and quota:** a new URL, such as moving the node under a path behind your proxy, is used for the next redirect and health probe. A smaller quota removes nothing; the node just caches no new title until it holds less.

**Viewers** are the Jellyfin users the router sends to an edge node. Assign them on the edge node's Edit page, picked from Jellyfin's user list, or from the CLI:

```sh
docker compose exec orchestrator orchestrator edge assign alice --node edge1
docker compose exec orchestrator orchestrator edge viewers
docker compose exec orchestrator orchestrator edge unassign alice
```

A user goes to one edge node at most; assigning them again moves them. Changes reach the router within about 10 seconds. `EDGE_VIEWERS` is no longer read: if your compose file still sets it, the orchestrator, the router and `doctor` say so. Assign those viewers here, then delete the line.

Rented servers' nodes are managed by the cloud scaler and cannot be edited. Names starting with `cloud-` are kept for them.

`node remove` revokes the node and forgets what it cached. Files stay on an edge node's disk until you delete them. Old jobs keep showing its name, and the name is free for a new node.

To stop a worker taking new jobs, pause it. It finishes the job it holds, then takes nothing until resumed. The pause is kept across restarts, and the Nodes page has the same buttons.

```sh
docker compose exec orchestrator orchestrator node pause worker1
docker compose exec orchestrator orchestrator node resume worker1
docker compose exec orchestrator orchestrator node pause --all
```

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
| `8200` | Control API, for workers, edge nodes and routers |
| `8202` | Admin UI |
| `8201` | Playback events from Jellyfin, in manual mode. Commented out by default |
| `8204` | Router, on the Jellyfin server |
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
| Admin UI | Put the path in `ADMIN_URL`, such as `https://example.com/jellyfinner-admin` |
| Jellyfin and the router | Set Jellyfin's Base URL under Dashboard, Networking, and include it in `JELLYFIN_URL`, such as `http://host.docker.internal:8096/jellyfin`. Proxy that path to the router or to Jellyfin |
| Edge node | Add it with `--url https://example.com/edge1`. `edge-serve` needs no setting |
| Webhook, manual mode | Proxy its own path to port `8201`, such as `https://example.com/hook`, and give Jellyfin the full URL, `https://example.com/hook/webhook/jellyfin`. No setting. It is not part of `CONTROL_URL` or `ADMIN_URL` |

HTTP/2 on your proxy is fine, such as nginx with `http2 on;`. File transfers use HTTP/1.1 on purpose, so parallel streams get their own connections, and every other request may use HTTP/2.

The control API and the admin UI need different paths, because each is its own listener.

The image trusts `X-Forwarded-For` from private addresses, so allowlists and failed-login limits see real clients behind a proxy. This is safe while ports stay on `127.0.0.1`. **If you publish the control API on a public address, set `TRUSTED_PROXIES` to your proxy's address, or to an empty value.** Otherwise, on a runtime that hides client addresses, an outsider could forge the header to get around those limits.

## Optional parts

Uncomment these blocks in the file.

| Block | Where | Gives you |
|---|---|---|
| `caddy` | Orchestrator, edge, router | Automatic HTTPS, if you have no reverse proxy. It needs ports 80 and 443 |
| `jellyfin` | Orchestrator | Runs Jellyfin, if you do not have one already |
| `ENCODER`, `devices`, `group_add` | Worker | Encodes with a VAAPI GPU, or with RKMPP on a Rockchip board running its vendor kernel. The worker image is built for amd64 and arm64 |
| `TRANSFER`, the `rclone` volume | Worker | Transfers with rclone over SFTP, for the speed test in [../first-run.md](../first-run.md) |

## Cloud workers

Optional. The orchestrator can rent a worker server by the hour when the queue backs up, and delete it once idle. Your own workers keep running as before. Jellyfinner has adapters for Cherry Servers (cloud VPS, hourly) and Hetzner Cloud. Neither is preferred: add none, one or both, and list them in `CLOUD_PROVIDERS` in the order to try them. When one cannot start a server, because of stock, quota or an outage, the next one is tried.

1. Create a project or account for Jellyfinner alone at each provider you add, and an API token for it. A Hetzner token can control its whole project.
2. In the orchestrator's compose file, set `CLOUD_PROVIDERS` and that provider's `CLOUD_<PROVIDER>_*` values. `CONTROL_URL` must be an https address that rented servers can reach. [.env.example](../../.env.example) lists every setting.
3. Run `docker compose exec orchestrator orchestrator doctor`. It checks each provider's token, project, types, locations and image.
4. Try one by hand: `orchestrator cloud up --provider NAME`, then watch `orchestrator cloud status`.

How it behaves:

- **Starting.** A server starts when the oldest waiting title has waited `CLOUD_WAIT` (10 minutes), or `CLOUD_QUEUE_THRESHOLD` titles wait (3). `CLOUD_MAX` caps how many run (1). Backfill alone does not start one unless `CLOUD_BACKFILL=true`.
- **Booting.** The server installs Docker if its image lacks it, then runs the worker with its own node token. A server that has not called in after `CLOUD_BOOT_TIMEOUT` is deleted.
- **Stopping.** Both providers bill a server until it is deleted, even when it is powered off, so Jellyfinner always deletes. An idle server is kept until just before its paid hour ends, because deleting it sooner saves nothing. Its node is paused first, so it cannot take a job while being deleted.
- **Making sure it is gone.** A server counts as deleted only once the provider confirms it is gone, along with any address it came with. Until then the delete is retried. After `CLOUD_DELETE_TIMEOUT` you get one notification, the Nodes page shows it in red, and `doctor` fails. Servers carry this orchestrator's tag, and a server without it is never deleted.
- **Limits.** `CLOUD_MAX_HOURS_PER_DAY` (8) caps billed hours per UTC day. `CLOUD_MAX_SPEND_PER_DAY` caps estimated spending, when every provider prices in one currency. `CLOUD_MAX_LIFETIME` drains any server after 12 hours.
- **Pausing.** `orchestrator cloud pause` stops new servers; running ones finish their jobs and are deleted as they go idle. Pausing a cloud server's node drains it.

```sh
docker compose exec orchestrator orchestrator cloud status
docker compose exec orchestrator orchestrator cloud estimate
docker compose exec orchestrator orchestrator cloud pause
docker compose exec orchestrator orchestrator cloud down --wait
```

Run `cloud down --wait` before removing a provider from `CLOUD_PROVIDERS`. The orchestrator refuses to start while a provider it no longer lists still has a server.

### Billing and estimates

Estimates use each provider's own prices from its API, excluding VAT and traffic, and each server keeps the price quoted when it started. Cost per title and the cost to clear the queue appear only after servers have finished real work; speed is measured, never guessed. Prices are shown for information and never change the order you set.

The **Usage** page in the admin UI, and `orchestrator usage [--month YYYY-MM]`, put it together by UTC month: what each worker and cloud type published and how fast, the traffic it moved, what the cloud cost, and how long and how much the remaining backlog would take.

| Provider | Partial hour | Powered off | Source, checked 2026-11-05 |
|---|---|---|---|
| Cherry Servers, cloud VPS, hourly | Not documented. Jellyfinner assumes a whole hour, the costlier case | Billed until terminated | [Server actions](https://www.cherryservers.com/knowledge/docs/compute/configuration-management/server-actions), [Cloud VPS](https://www.cherryservers.com/knowledge/docs/compute/virtual-servers/cloud-vps) |
| Hetzner Cloud | Rounded up to a whole hour, capped at the monthly price | Billed until deleted | [Hetzner Cloud billing FAQ](https://docs.hetzner.com/cloud/billing/faq/) |

## Manual mode

Manual mode encodes a title when someone presses play on its stub, so Jellyfin must be able to tell the orchestrator. Set these in `compose.orchestrator.yml` before the first start:

- `ENCODE_MODE: manual`
- `JELLYFIN_TOKEN`, which manual mode requires
- `WEBHOOK_TOKEN`, at least 32 characters, such as the output of `openssl rand -hex 32`

Then install the Webhook plugin in Jellyfin and add a Generic destination:

| Setting | Value |
|---|---|
| Webhook URL | `http://orchestrator:8201/webhook/jellyfin` for Jellyfin on the `jellyfinner` network. For Jellyfin on the host, uncomment port `8201` and use `http://127.0.0.1:8201/webhook/jellyfin`. For Jellyfin on another machine, see below |
| Notification type | Playback Start |
| Template | The contents of [../jellyfin-webhook.json](../jellyfin-webhook.json) |
| Header | `X-Webhook-Token`, with your `WEBHOOK_TOKEN` |

Keep the port on `127.0.0.1` or a private address. To change the mode of a running deployment, use `orchestrator switch-mode`, with `serve` stopped.

**Jellyfin on another machine.** The webhook is its own listener on port `8201`. It is not served at `CONTROL_URL` or `ADMIN_URL`: a playback event sent there gets 401 or 404. Uncomment `127.0.0.1:8201:8201`, and give it its own HTTPS path on your proxy, such as `https://example.com/hook`:

```nginx
location /hook/ {
    proxy_pass http://127.0.0.1:8201;
}
```

Then set the Webhook URL to `https://example.com/hook/webhook/jellyfin`. The listener answers with or without the prefix, so the proxy may keep it or strip it. Over the internet the `X-Webhook-Token` header is all that guards this URL, so keep `WEBHOOK_TOKEN` long and random, and if you can, let only Jellyfin's address reach the path, such as with nginx `allow` and `deny`. A private network such as WireGuard between the two machines works too: then use the orchestrator's private address, as `http://10.8.0.1:8201/webhook/jellyfin`, and publish the port on that address instead of `127.0.0.1`.

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
