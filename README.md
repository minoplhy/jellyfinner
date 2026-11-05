# Jellyfinner

Remote encoding for Jellyfin, for when your server and your compute are in different places.

Stretching Jellyfin's transcoder across a slow link is fragile, so Jellyfinner treats compute as a batch encoding farm. A worker pulls the whole file, encodes it, verifies it, and publishes a direct-play MP4 back into the library. Jellyfin streams finished files and never transcodes.

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

The first scan that finds files records your existing library and queues nothing, so an empty or wrongly mounted folder cannot start the whole library later. In `auto`, encoding it is a separate step, `orchestrator backfill start`, with a daily traffic budget.

## Output

One MP4 that every client can direct play: H.264 8-bit up to 1080p at about 8 Mbps, with AAC stereo audio. HDR is tonemapped to SDR, and text subtitles become sidecar files. ASS tracks also keep a styled `.ass` copy.

In this version, surround audio is dropped, image subtitles such as PGS are not served, and there is no lower-bitrate version for slow connections.

## Getting started

Each role is one compose file. Paste [the orchestrator](deploy/docker/compose.orchestrator.yml), [worker](deploy/docker/compose.worker.yml) or [edge](deploy/docker/compose.edge.yml) file into your own compose file, change the values marked `EDIT`, and run `docker compose up -d`. It needs no script, `.env` or other files.

Add a node from the orchestrator. It prints the settings to paste, and works with no restart:

```sh
docker compose exec orchestrator orchestrator node add worker1
```

- [First deployment](deploy/first-run.md): every stage, with a check for each
- [Docker Compose](deploy/docker/README.md): the main path
- [systemd on the host](deploy/README.md): the alternative

`docker compose run --rm SERVICE doctor` checks a node from inside its container. [.env.example](.env.example) lists every setting.

## Everyday use

The admin UI covers the queue, library and nodes, and does what the CLI does: add, rotate and remove nodes, start and pause backfill, and scan now. Set `ADMIN_ADDR` to enable it, and `ADMIN_URL` to the address people open. Login uses Jellyfin, for administrators only.

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

orchestrator node add worker2
orchestrator node list
orchestrator node rotate worker2
orchestrator node remove worker2
orchestrator node pause worker2        # finishes its job, then takes no more
orchestrator node resume worker2

orchestrator cloud status              # rented workers, if CLOUD_PROVIDERS lists any
orchestrator cloud estimate
orchestrator cloud pause

orchestrator switch-mode manual
```

Cloud workers are optional: the orchestrator can rent servers by the hour from the providers you add, and delete them when idle. See [Cloud workers](deploy/docker/README.md#cloud-workers).

Source paths are relative to `ORIGINALS_DIR`. `switch-mode` needs `ENCODE_MODE` changed first, and `serve` stopped.

## Edge delivery (optional)

For viewers far from Jellyfin. You choose titles to cache on a small node nearby, and the router redirects assigned viewers to it, while browsing and watch progress stay on Jellyfin. If anything looks wrong, playback simply comes from the origin.

Edge nodes work behind any reverse proxy, or use the commented `caddy` block in the compose file. Read [deploy/edge-security.md](deploy/edge-security.md) before enrolling a node.

## Development

Needs Go 1.27, plus gcc for the orchestrator and router, which use SQLite. The tests also need ffmpeg and rclone, and rclone stays until the transfer speed test decides.

```sh
GOFLAGS=-p=1 go build ./...
GOFLAGS=-p=1 go test -p 1 ./...
```

`worker` and `edge-agent` build as static binaries with `CGO_ENABLED=0`. Keep them free of `internal/database`, which pulls in SQLite.

### Code layout

The code is in layers. Each layer talks to the next one through an interface.

| Layer | Packages | Rule |
|---|---|---|
| Records | `internal/model` | Plain types, states and errors. No storage code. |
| Storage | `internal/database` (`Store` interface), `internal/database/sqlite` | Every SQL statement lives in a backend. Nothing else imports `database/sql` or a driver. |
| Services | `internal/orchestrator`, `internal/nodes`, `internal/watch` | Rules and background work, through `database.Store`. |
| Edges | `internal/api`, `internal/web`, `internal/edgerouter` | HTTP handlers that call services and `database.Store`. |
| Outside systems | `jellyfin.API`, `control.API`, `transfer.Transfer`, `notify.Notifier`; ffmpeg in `internal/encode` | One interface per package, implemented by the real client. Tests embed `jellyfintest.Unimplemented` or `controltest.Unimplemented`. |
| Wiring | `cmd/*` | The only place that picks a backend, through `openStore` in `cmd/orchestrator/store.go`. |

To add a backend such as MariaDB:

1. Create `internal/database/mysql`.
2. Implement `database.Store` in it.
3. Call `database.Register` from its `init`.
4. Pass `dbtest.Run` in its tests.
5. Choose it in `openStore`.

`internal/archtest` fails the build if these rules are broken.

## Documents

- [deploy/first-run.md](deploy/first-run.md): first deployment, stage by stage
- [deploy/docker/README.md](deploy/docker/README.md): Docker Compose
- [deploy/README.md](deploy/README.md): host install, backfill and cutover
- [deploy/proxy/README.md](deploy/proxy/README.md): reverse proxy examples
- [deploy/edge-security.md](deploy/edge-security.md): edge security review
