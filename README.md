# Jellyfinner

Remote encoding for Jellyfin, for when your server and your compute are in different places.

Stretching Jellyfin's transcoder across a slow link is fragile, so Jellyfinner treats compute as a batch encoding farm. A worker pulls the whole file, encodes it, verifies it, and publishes a direct-play MP4 back into the library. Jellyfin streams finished files and never transcodes.

> **Status:** all parts have run together in containers on one machine, with real Jellyfin, real reverse proxies, two workers and two edge nodes. Nothing has run on separate servers or real client apps yet. See [planning.md](planning.md) for what still needs proving.

## How it works

Jellyfinner has three roles. Any role can run on any machine in any region, and several can share one machine. You can run many workers and edge nodes.

```
 Orchestrator node                               Worker node
 originals                                       worker + ffmpeg
     | watcher                                       ^
     v                                               |  jobs, source, encodes
 orchestrator (SQLite queue) <- HTTPS or WireGuard ->+
     |  verify, publish, notify
     v                                           Edge node (optional)
 library -> Jellyfin -> router  ---- redirect -> edge-agent + reverse proxy
                                                 caches chosen titles
```

Workers and edge nodes connect out to the orchestrator, using one API with per-node tokens. If a worker goes offline, new titles wait in the queue and finished titles keep playing.

| Program | Role | Purpose |
|---|---|---|
| `orchestrator` | Orchestrator node | Watches originals, runs the queue, publishes encodes, serves the admin UI, and is the CLI. `orchestrator router` optionally sends viewers to a nearby cached copy |
| `worker` | Worker node | Pulls a source, encodes, verifies and pushes the result |
| `edge-agent` | Edge node | Optional. `pull` fetches and removes cached files on order, and `serve` answers viewers behind any reverse proxy |

## Modes

Exactly one is active, set by `ENCODE_MODE`. There is no default.

| | `auto` | `manual` |
|---|---|---|
| New file | Encoded automatically | Gets a "Not ready yet" stub |
| What starts an encode | The watcher | Pressing play on a stub, the UI, or the CLI |
| Cost | Every title is moved and encoded | Only titles someone wants |
| First play | Immediate once encoded | Waits for the encode |

The first scan records your existing library and queues nothing. In `auto`, encoding it is a separate step, `orchestrator backfill start`, with a daily traffic budget.

## Output

One MP4 that every client can direct play: H.264 8-bit up to 1080p at about 8 Mbps, with AAC stereo audio. HDR is tonemapped to SDR, and text subtitles become sidecar files. ASS tracks also keep a styled `.ass` copy.

In this version, surround audio is dropped, image subtitles such as PGS are not served, and there is no lower-bitrate version for slow connections.

## Getting started

Each node takes three commands: copy `deploy/docker/`, run `./init.sh` for its role, then `docker compose up -d`.

- [First deployment](deploy/first-run.md): every stage, with a check for each
- [Docker Compose](deploy/docker/README.md): the main path
- [systemd on the host](deploy/README.md): the alternative

[.env.example](.env.example) lists every setting.

## Everyday use

The admin UI covers the queue, library and nodes. Set `WEB_ADDR` to enable it. Login uses Jellyfin, for administrators only.

```sh
orchestrator queue list
orchestrator queue add "Movies/Dune (2021)/Dune (2021).mkv"
orchestrator queue reencode "Movies/Dune (2021)/Dune (2021).mkv" --reason "audio out of sync" --force-encode
orchestrator queue cancel JOB
orchestrator queue retry JOB

orchestrator backfill start
orchestrator backfill pause
orchestrator backfill status

orchestrator edge pin "Movies/Dune (2021)/Dune (2021).mkv" --node edge1
orchestrator edge list

orchestrator switch-mode manual
```

Source paths are relative to `ORIGINALS_DIR`. `switch-mode` needs `ENCODE_MODE` changed first, and `serve` stopped.

## Edge delivery (optional)

For viewers far from Jellyfin. You choose titles to cache on a small node nearby, and the router redirects assigned viewers to it, while browsing and watch progress stay on Jellyfin. If anything looks wrong, playback simply comes from the origin.

Edge nodes work behind any reverse proxy, or use the bundled Caddy profile. Read [deploy/edge-security.md](deploy/edge-security.md) before enrolling a node.

## Development

Needs Go 1.27, plus gcc for the orchestrator and router, which use SQLite. The tests also need ffmpeg and rclone, and rclone stays until the transfer speed test decides.

```sh
GOFLAGS=-p=1 go build ./...
GOFLAGS=-p=1 go test -p 1 ./...
```

`worker` and `edge-agent` build as static binaries with `CGO_ENABLED=0`. Keep them free of the `queue` package, which pulls in SQLite.

## Documents

- [planning.md](planning.md): phases, status and known gaps
- [deploy/first-run.md](deploy/first-run.md): first deployment, stage by stage
- [deploy/docker/README.md](deploy/docker/README.md): Docker Compose
- [deploy/README.md](deploy/README.md): host install, backfill and cutover
- [deploy/proxy/README.md](deploy/proxy/README.md): reverse proxy examples
- [deploy/edge-security.md](deploy/edge-security.md): edge security review
