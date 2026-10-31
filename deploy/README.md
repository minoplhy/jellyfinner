# Deploying Jellyfinner on the host

For operators who prefer systemd to containers. Docker Compose is the main path: see `first-run.md` and `docker/README.md`. The backfill and cutover steps below apply to both.

Roles can run on any machines. Follow `first-run.md` for the order and checks.

## Orchestrator node

1. Create a `jellyfinner` user, and `/srv/originals`, `/srv/library` and `/srv/incoming`. Incoming and library must be on one filesystem.
2. Move nothing yet. Point `ORIGINALS_DIR` at the existing media.
3. Copy `.env.example` to `/etc/jellyfinner/orchestrator.env`, mode 600, and fill it in.
4. Install the binary and `systemd/jellyfinner-orchestrator.service`.
5. Apply `sysctl/90-jellyfinner.conf`, and set up WireGuard from `wireguard/` if used.

If `/srv/originals` differs, update `RequiresMountsFor`, `ReadOnlyPaths` and `ReadWritePaths` in the unit.

## Worker node

1. Install jellyfin-ffmpeg.
2. Copy `worker.env.example` to `/etc/jellyfinner/worker.env`, mode 600.
3. Install the binary and `systemd/jellyfinner-worker.service`. It transfers files over the control API, so it needs no SSH access. Set `--backend` to match the host: `software` by default, or `vaapi` with a GPU.
4. Apply the same sysctl file.

## Admin UI

Set `WEB_ADDR`. Either give it `WEB_TLS_CERT` and `WEB_TLS_KEY`, or bind a private address behind an HTTPS reverse proxy that sets `X-Forwarded-Proto: https`, so the session cookie is marked Secure. Keep the control API on its own listener.

## Edge delivery

Read `edge-security.md` first. Files are stored by content hash, and only finished encodes ever reach a node.

On the orchestrator node:

1. Add `EDGE_NODES`, `EDGE_VIEWERS` and `EDGE_TOKEN_KEY`, and a `NODE_TOKENS` entry for each node.
2. Install `systemd/jellyfinner-edge-router.service`, which runs `orchestrator router`, and point the public Jellyfin hostname at `ROUTER_ADDR`. Every Jellyfin request then passes through it.
3. Add the router's address to Jellyfin's known proxies, so viewers' real addresses are kept.

On each node:

1. Build the agent with `CGO_ENABLED=0 go build ./cmd/edge-agent`. One static binary serves every distribution.
2. Create users `edge-agent` and `edge-serve`, and a group `nginx`, or the group of your choice if you edit the units. `/edge` is owned by `edge-agent:nginx`, mode 2750, so `edge-serve` can read and only the agent can write.
3. Copy `edge-agent.env.example` to `/etc/jellyfinner/edge-agent.env`, and `edge-serve.env.example` to `/etc/jellyfinner/edge-serve.env`, both mode 600.
4. Install both services: `systemd/jellyfinner-edge-agent.service` and `jellyfinner-edge-serve.service`, or the `openrc/` scripts on Alpine.
5. Point any reverse proxy at `127.0.0.1:8205`. See `proxy/README.md`. Instead of `edge-serve`, nginx can check signatures itself with `nginx/edge.conf`.

Then cache a title with the Library page or `orchestrator edge pin SOURCE --node edge1`.

## Backfill and cutover (`auto`)

Nothing queues the existing library until you ask.

1. Add a new Jellyfin library pointing at `/srv/library`, visible only to test users.
2. Run `orchestrator backfill status` to see the work and traffic.
3. Try a few titles with `orchestrator queue add`, and test them on real clients.
4. Run `orchestrator backfill start`. Use `pause` at any time. Queued backfill jobs stop being claimed at once, and a job already running finishes.
5. A message says when backfill has finished. Check for failed titles in the Queue page.

Cutover, which changes what viewers see:

1. Give all users access to the new library, and remove the old one.
2. For every user, including defaults for new users, disable video transcoding, audio transcoding and remux.
3. In library settings, turn off trickplay, chapter image extraction and keyframe extraction.
4. Confirm with a real playback that no ffmpeg process runs on the Jellyfin host.

Watch history does not carry over, because the new library has new paths.
