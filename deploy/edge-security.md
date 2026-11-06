# Edge agent security review

For whoever approves enrolling a real edge node. This review was done by the author of the code, so it needs a second reader first.

Result: no known blocking issue in the code. The items below must still be checked on a real node. So far it has run in containers on one machine, not on separate servers.

The node runs one binary as two processes. `edge-agent pull` connects out, can write `/edge`, and holds the node token. `edge-agent serve` listens behind a reverse proxy, can only read `/edge`, and holds only the signing key. A flaw in the listening process therefore cannot reach the token or change files.

## What was checked

| Claim | How it was tested |
|---|---|
| `pull` has no listening port | Checked with `ss` on the running agent |
| `serve` refuses anything unsigned | Altered and wrong-key URLs got 403, expired ones 410, and missing signatures 403. `/edge/.tmp`, `/` and traversal paths got 404. Same results behind Caddy, behind nginx, and with nginx `secure_link` |
| Worker transfers reach only the current job | Wrong lease, no lease, another job, another node and a finished job were all refused. Names that escape the job folder were refused. An edge token cannot reach them |
| Orders cannot name a path or URL | The order type has no such field. Traversal strings, uppercase and malformed hashes, and unknown verbs were refused with nothing touched |
| Nothing unverified is served | A tampered pull left no `.mp4`, and the partial was discarded |
| A node takes only what was ordered | Bytes beyond the ordered size fail the pull |
| Download tokens cover one node and one file | A different file, node, key and expiry were all refused, and so was a forged later expiry. A token stops working once the pull ends |
| Nodes see only their own orders | One node could not activate another's file |
| Roles are separate | An edge token got 403 on job claims, and a worker token got 403 on edge routes |
| Viewers' Jellyfin tokens stay on the orchestrator node | The redirect URL was checked for the token |
| `/health` shows the build only to the orchestrator | Plain, wrong-key and expired probes got `ok` with no build header. Only a probe signed with the node's key, as the orchestrator sends every 30 seconds, gets `X-Jellyfinner-Build`. Covered by a unit test, not yet on a real node |
| Router faults fall back to the origin | Bad token, unhealthy node, silent agent and a closed database all passed through |

## What must be checked on a real node

1. **Sandbox.** The systemd and OpenRC limits have never been applied. Confirm writes outside `/edge` fail, and that `NoNewPrivs` is set in `/proc/<pid>/status`.
2. **Your reverse proxy.** The checks above ran behind Caddy and nginx. If you use another, confirm it keeps the path and query unchanged. Expired and altered URLs must be refused, and `/edge/.tmp` must not be reachable.
3. **Dependencies.** The agent uses only the Go standard library. Run `govulncheck` on the release build.

## Accepted risks

- **Node compromise** exposes the encoded files on that node, and lets the attacker mint URLs for that node. It does not reach Jellyfin, viewers' tokens, originals or other nodes.
- **Stolen agent token** gives access to that node's orders and the files ordered to it. Add a source IP allowlist with `node add --allow`. `node remove` or `node rotate` revokes it within seconds.
- **The orchestrator's database holds each node's signing key in the clear,** because it must sign URLs, along with the edge token key. Node tokens are stored only as hashes. Keep the state folder private, and treat backups of it as secrets.
- **Node secrets can sit in a compose file,** which is the cost of a one-file deployment. Keep that file private and out of git, or use `${VAR}` references to your own secret store.
- **Orchestrator compromise** can make a node download from it or delete cached files. It gets no shell on the node, and the loss is a cache.
- **Signed URLs use MD5,** the only option in nginx's `secure_link`, kept so nginx stays usable. A long random per-node key is required.
- **Jellyfin serves direct streams without a token.** On Jellyfin 12.4, a stream URL for a known item ID worked with no token at all. This is Jellyfin's behaviour, not added by Jellyfinner. Item IDs are hard to guess, but treat them as access to that title.
- **Signed URLs stay valid until expiry,** 24 hours by default, so revoking a viewer does not end access at once. Lower `EDGE_URL_TTL` to shorten this, keeping it longer than the longest title.
- **Agent updates are manual.** There is no automatic update path, so a fix must be rolled out by hand.
