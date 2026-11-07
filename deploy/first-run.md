# First deployment

For the operator bringing up a real deployment. It uses Docker Compose, and goes one stage at a time, so a failure points at one part. Do not move on until a stage's check passes.

Roles below are the orchestrator node, worker node and edge node. They can be on any machines.

Each node needs only its compose file, from [docker/README.md](docker/README.md). After starting a node, `docker compose run --rm SERVICE doctor` checks it from inside its container, so run that first whenever a stage fails.

## 1. Network

Choose how nodes reach the control API.

- **HTTPS, the simple option.** Give it a hostname, and use the `caddy` block or your own proxy. Nodes need no open ports except an edge node's HTTPS.
- **WireGuard.** Useful if a node's IP keeps changing. Publish port 8200 on the private address, and use an `http://` URL on that address. See `wireguard/`.

No SSH or SFTP access between nodes is needed.

Apply `sysctl/90-jellyfinner.conf` on each host if nodes are far apart.

**Check:** from a worker and an edge node, this prints 401, which proves the route works:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://control.example.com/v1/jobs/claim
```

## 2. Orchestrator node

Follow the orchestrator setup in [docker/README.md](docker/README.md). Mount a test folder holding one episode as `/originals`, not your whole library. Do not start backfill.

**Check:** `docker compose run --rm orchestrator doctor` passes, `docker compose ps` shows `orchestrator` as healthy, and the log says the first scan recorded your file as backlog.

## 3. Worker node

Run `docker compose exec orchestrator orchestrator node add worker1` on the orchestrator. Put the printed values into the worker's compose file, then run `docker compose up -d` there. The orchestrator needs no restart.

**Check:** `docker compose run --rm worker doctor` passes on the worker.

Queue the episode:

```sh
docker compose exec orchestrator orchestrator queue add "Show/Episode 01.mkv"
docker compose exec orchestrator orchestrator queue list
```

**Check:** the job reaches `done`, and `Episode 01.mp4` with any `.srt` and `.ass` files appears under the `library` folder of your store.

The job's log, on the admin UI's Queue page, records pull and push speed in MB/s.

### Transfer speed test

Workers transfer over HTTPS by default. To compare with rclone on your real link:

1. Run a job with the default, and note the speeds in its log.
2. Set up an rclone remote named `origin` over SFTP, in a `./rclone` folder on the worker. Set `REMOTE_ORIGINALS` and `REMOTE_INCOMING` on the orchestrator.
3. Uncomment `TRANSFER: rclone` and the `rclone` volume in the worker's compose file, and re-encode the same title.

Keep whichever is faster. Paths in rclone must be the ones the SSH user sees.

### Rockchip hardware encoding (optional)

On a Rockchip board such as an RK3588, the worker can encode with RKMPP. The board must run Rockchip's vendor kernel, because mainline kernels have no `/dev/mpp_service`.

1. Uncomment `ENCODER: rkmpp` and the RKMPP `devices` in the worker's compose file. Set `group_add` to the host's `video` group ID (`getent group video | cut -d: -f3`), not `render`: `video` owns `/dev/mpp_service`, `/dev/rga` and `/dev/dma_heap`. Check with `ls -l` on those nodes if unsure.
2. **Check:** `docker compose run --rm worker doctor` passes the Rockchip device, encoder and test encode checks. The device check opens each node, so a wrong group fails here with the group ID to add.
3. Re-encode the test episode. **Check:** its manifest on the Queue page says `backend: rkmpp`, not `software`.

If the hardware path fails, the worker doesn't fail the job. It first retries with the CPU decoding and the hardware still encoding, which covers a hardware decoder that rejects a codec, such as HEVC on some RK3576 vendor kernels. If that fails too, it encodes in software. The job log has a `backend rkmpp failed:` line with ffmpeg's error for each failed attempt, and a full software fallback shows as `software (fallback)` on the Nodes page. `ps -eo args | grep [f]fmpeg` on the board shows `h264_rkmpp` for a hardware encode and `libx264` for software. To see whether the video engine is busy, watch `/proc/mpp_service/load` after `echo 1000 > /proc/mpp_service/load_interval`, or watch the `rkvenc`/`rkvdec` counts in `/proc/interrupts`. The Mali GPU stays idle either way, because RKMPP doesn't use it.

To prove each new image on the board, enrol it as a self-hosted GitHub Actions runner with the labels `linux`, `arm64` and `rkmpp`, then start the Images workflow by hand with `hardware: rkmpp`. That job never runs for pull requests, so code from strangers never reaches the board.

## 4. Jellyfin

Add a new library pointing at the `library` folder of your store, and create a test user with video transcoding, audio transcoding and remux all disabled. Leave any existing library alone.

**Check:** the test user plays the episode on each app you use. In a browser's Network tab, a single `stream` request means direct play. Numbered `.ts` or `.mp4` chunks, or `/hls1/` in the URL, mean the server is transcoding. Try the `.ass` subtitle track, and check signs and positioning.

**In manual mode,** set up the webhook as in [docker/README.md](docker/README.md#manual-mode).

**Check:** the test user presses play on a stub, and `queue list` shows a new job for that title.

## 5. Edge node

Run `docker compose exec orchestrator orchestrator node add edge1 --edge --url https://edge1.example.com --quota-gb 500` on the orchestrator. Put the printed values into the edge node's compose file, then run `docker compose up -d` there. Give the node HTTPS with the `caddy` block or your own proxy.

Pin the episode:

```sh
docker compose exec orchestrator orchestrator edge pin "Show/Episode 01.mkv" --node edge1
docker compose exec orchestrator orchestrator edge list
```

**Check:** both `doctor` commands in [docker/README.md](docker/README.md) pass, `edge list` shows `active`, and `https://edge1.example.com/health` returns `ok`.

## 6. Router

Assign the test user to the edge node, on its Edit page under Viewers, or with `docker compose exec orchestrator orchestrator edge assign testuser --node edge1`. Add a router with `docker compose exec orchestrator orchestrator node add jf-router --router`, then on the Jellyfin server fill in [docker/compose.router.yml](docker/compose.router.yml) and run `docker compose up -d` and `docker compose run --rm router doctor`. Keep your public hostname pointed at Jellyfin for now, and test against port 8204 on the Jellyfin server.

**Check:** with the test user's token, a stream request returns a 302 to the edge node:

```sh
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' \
  "http://127.0.0.1:8204/Videos/ITEM_ID/stream.mp4?Static=true&ApiKey=USER_TOKEN"
```

Then point the public hostname at the router, add that address to Jellyfin's known proxies, and test on your real apps, including seeking and resume. An app that fails on the redirect stays on the origin: remove that viewer from the edge node, which takes effect within seconds.

## 7. Failure drills

| Do this | Expect |
|---|---|
| Stop the worker mid-job | After the lease expires, about 3 minutes, the job restarts on another worker or waits |
| Stop the edge node | Within about 30 seconds, the router sends viewers to the origin. It returns when the node recovers |
| Re-encode a pinned title | Viewers fall back to the origin, then the node holds only the new file |
| Restart the orchestrator | Jellyfin stays reachable through the router |

After this, follow backfill and cutover in [README.md](README.md). Keep the daily budget small on a CPU-only worker.
