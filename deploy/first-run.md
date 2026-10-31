# First deployment

For the operator bringing up a real deployment. It uses Docker Compose, and goes one stage at a time, so a failure points at one part. Do not move on until a stage's check passes.

Roles below are the orchestrator node, worker node and edge node. They can be on any machines.

Before starting, run the read-only preflight on each node. It changes nothing.

```sh
deploy/check.sh orchestrator
deploy/check.sh worker
deploy/check.sh edge
```

## 1. Network

Choose how nodes reach the control API.

- **HTTPS, the simple option.** Give it a hostname, and use the `caddy` profile or your own proxy. Nodes need no open ports except an edge node's HTTPS.
- **WireGuard.** Useful if a node's IP keeps changing. Set `CONTROL_ADDR` to the private address, and use an `http://` URL on that address. See `wireguard/`.

No SSH or SFTP access between nodes is needed.

Apply `sysctl/90-jellyfinner.conf` on each host if nodes are far apart.

**Check:** from a worker and an edge node, this prints 401, which proves the route works:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://control.example.com/v1/jobs/claim
```

## 2. Orchestrator node

Follow the orchestrator setup in [docker/README.md](docker/README.md). Point `ORIGINALS_HOST` at a test folder holding one episode, not your whole library. Do not start backfill.

**Check:** `docker compose ps` shows `orchestrator` as healthy, and the log says the first scan recorded your file as backlog.

## 3. Worker node

Run `./init.sh add-worker worker1` on the orchestrator, run the command it prints on the worker, then `sudo ./init.sh dirs` there, then `docker compose up -d` on both.

Queue the episode:

```sh
docker compose exec orchestrator orchestrator --env /nonexistent queue add "Show/Episode 01.mkv"
docker compose exec orchestrator orchestrator --env /nonexistent queue list
```

**Check:** the job reaches `done`, and `Episode 01.mp4` with any `.srt` and `.ass` files appears under `STORE_HOST/library`.

The job's log, on the admin UI's Queue page, records pull and push speed in MB/s.

### Transfer speed test

Workers transfer over HTTPS by default. To compare with rclone on your real link:

1. Run a job with the default, and note the speeds in its log.
2. Set up an rclone remote named `origin` over SFTP, in `RCLONE_CONFIG_HOST` on the worker. Set `REMOTE_ORIGINALS` and `REMOTE_INCOMING` on the orchestrator.
3. Add `compose.worker-rclone.yml` to `COMPOSE_FILE` on the worker, and re-encode the same title.

Keep whichever is faster. Paths in rclone must be the ones the SSH user sees.

## 4. Jellyfin

Add a new library pointing at `STORE_HOST/library`, and create a test user with video transcoding, audio transcoding and remux all disabled. Leave any existing library alone.

**Check:** the test user plays the episode on each app you use. In a browser's Network tab, a single `stream` request means direct play. Numbered `.ts` or `.mp4` chunks, or `/hls1/` in the URL, mean the server is transcoding. Try the `.ass` subtitle track, and check signs and positioning.

## 5. Edge node

Run `./init.sh add-edge edge1 https://edge1.example.com 500` on the orchestrator, run the printed command on the edge node, then `sudo ./init.sh dirs` there, then `docker compose up -d` on both. Give the node HTTPS with the `caddy` profile or your own proxy.

Pin the episode:

```sh
docker compose exec orchestrator orchestrator --env /nonexistent edge pin "Show/Episode 01.mkv" --node edge1
docker compose exec orchestrator orchestrator --env /nonexistent edge list
```

**Check:** `edge list` shows `active`, and `https://edge1.example.com/health` returns `ok`.

## 6. Router

Set `EDGE_VIEWERS=testuser=edge1`, add `router` to `COMPOSE_PROFILES`, and run `docker compose up -d`. Keep your public hostname pointed at Jellyfin for now, and test against `ROUTER_ADDR`.

**Check:** with the test user's token, a stream request returns a 302 to the edge node:

```sh
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' \
  "http://127.0.0.1:8204/Videos/ITEM_ID/stream.mp4?Static=true&ApiKey=USER_TOKEN"
```

Then point the public hostname at `ROUTER_ADDR`, add that address to Jellyfin's known proxies, and test on your real apps, including seeking and resume. An app that fails on the redirect stays on the origin: remove that viewer from `EDGE_VIEWERS`.

## 7. Failure drills

| Do this | Expect |
|---|---|
| Stop the worker mid-job | After the lease expires, about 3 minutes, the job restarts on another worker or waits |
| Stop the edge node | Within about 30 seconds, the router sends viewers to the origin. It returns when the node recovers |
| Re-encode a pinned title | Viewers fall back to the origin, then the node holds only the new file |
| Restart the orchestrator | Jellyfin stays reachable through the router |

After this, follow backfill and cutover in [README.md](README.md). Keep the daily budget small on a CPU-only worker.
