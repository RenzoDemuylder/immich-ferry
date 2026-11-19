# immich-ferry

A small daemon that keeps albums in sync, in both directions, between two [Immich](https://immich.app) instances.
It talks only to the REST API of each server (no database or filesystem access), runs unattended, and ships as a distroless
container image and a Helm chart.

## Features

- Bidirectional album sync: assets added to either album are copied (original file, stream-verified by SHA1) to the other.
- Choose the albums: configured album ids, **every album** the user can edit, or **albums marked with a symbol** (e.g. an emoji)
  in their description. Discovered albums are paired by name; a missing counterpart is created.
- Removals propagate as "unlink from the album" only, after a per-item probe confirms the asset really left. **Assets are never deleted.**
- Live photos (still + motion part) are copied and linked; Android motion photos are copied as the still only.
- Metadata sync: description, date/time and offset, location, rating, archive/timeline visibility, plus album name and description.
  Favorites are per user in Immich and are not synced.
- Restores a matching asset from the target's trash (except during the first bootstrap cycle).
- Safe by default: mass-removal guard, per-item failure backoff, write-ahead intents, atomic state, graceful shutdown.
- Prometheus metrics, `/healthz`, `/readyz`, `/status`; JSON logs.

## How it works

Each cycle, per album pair: **preflight** (version, API key permissions, albums) -> **snapshot** both albums -> **plan** ->
**removals** -> **metadata** -> **album info** -> **copies** (bounded by a time budget; the rest continues next cycle).

Assets are identified by their content checksum. A per-pair state file remembers which items were in both albums after the last
successful cycle (a 3-way diff):

| in A | in B | in state | action |
|---|---|---|---|
| yes | yes | any | paired: merge metadata, remember |
| yes | no | no | copy A -> B |
| no | yes | no | copy B -> A |
| yes | no | yes | candidate removal on B: probe B, then unlink from A |
| no | yes | yes | candidate removal on A: probe A, then unlink from B |
| no | no | yes | forget |

An empty state (first run, lost or reset) never removes anything; at worst items are re-added.

The removal probe asks the server directly whether the asset is still a member of the album. Not a member, or a member that is
trashed, propagates the unlink. A hidden member (typically Immich auto-linked it as a live-photo motion part) is never propagated;
an offline asset or an item that is still visible (snapshot glitch) is skipped for this cycle.

**Metadata merge** keeps a per-side last-observed baseline for every paired asset. A side "changed" when its value differs from its
own baseline; only changed fields are written, and the baseline is set from the server read-back, so there is no ping-pong. If both
sides changed the same field, `SYNC_CONFLICT_WINNER` (default `a`) wins. Without a baseline (new pairing), description, rating and
location only fill an empty side; date and visibility are just recorded (divergence is logged). Only the asset owner can
write; other owners' assets are skipped. Just-uploaded assets use source-wins for EXIF fields until the target has extracted its
own metadata (or `SYNC_PENDING_META_TIMEOUT`).

### Choosing albums

`SYNC_DISCOVERY` selects which albums are synced:

| Mode | Albums | Configure |
|---|---|---|
| `off` (default) | the configured pairs | `SYNC_A_ALBUM_ID` + `SYNC_B_ALBUM_ID`, or `pairs` in the YAML file |
| `all` | every album the API key's user owns or was shared as editor (viewer-only albums are skipped) | `SYNC_DISCOVERY=all` |
| `marker` | albums whose description contains `SYNC_DISCOVERY_MARKER` (default `🔁`) on either side | `SYNC_DISCOVERY=marker` |

A dedicated Immich user per instance that only owns, or is shared, the albums to sync works well with `all`.

With discovery, each cycle lists the albums on both sides:

- An album on one side is paired with the album of **exactly the same name** on the other side. The pair is then remembered by
  album id (in `discovery.json` in the state directory), so renaming an album later does not break it.
- No album of that name on the other side: one is **created** with the same name and description (the marker comes along), owned
  by the API key's user. Album creation is never retried within a cycle; an album created by a failed-looking request is adopted
  by name in the next cycle.
- Immich cannot transfer album ownership, so with a dedicated sync user the created album belongs to that user. Set
  `SYNC_DISCOVERY_SHARE_A` / `SYNC_DISCOVERY_SHARE_B` to the people (email or user id) on that side who should see it: the album
  is created already shared with them as `SYNC_DISCOVERY_SHARE_ROLE` (default `editor`). An email that does not match a user
  means the album is not created (reported as an error, retried next cycle), so a counterpart is never created unshared.
  Only albums created by discovery are shared; existing albums and their members are never changed.
  Emails are looked up in the user list (`GET /users`); if the server does not list other users, use user ids.
- **Everyone left a shared album → teardown.** When every user an album created by discovery was shared with has left it (or
  their account was deleted), the sync user stops syncing it, in this order: it **leaves** the original album on the other side
  (if it owns that album it cannot leave, and keeps the emptied counterpart instead), with `SYNC_DISCOVERY_TEARDOWN_TRASH=true`
  moves the photos **it uploaded** into the counterpart **to the trash** (only those in no other album it can see), **deletes**
  the counterpart (Immich keeps its photos) and forgets the pair. It is checked once per cycle (`SYNC_INTERVAL`), at no extra API
  cost; a listing that shows no members is double-checked by id first. If one of several users is still a member, nothing
  happens. A failed step is retried next cycle; the pair is never synced again. Trash is recoverable by the sync user only, for
  Immich's trash retention. **Caveat for trashing:** a member can add the sync user's photos to an album of their own, which the
  sync user cannot see; those photos then vanish from that album too. That is why trashing is off by default.
  Pairs from older versions are recorded as "created" when the sync user owns exactly one of the two albums and it has members.
  Set `SYNC_DISCOVERY_TEARDOWN=false` to turn the teardown off: such a pair then simply keeps syncing (the key no longer needs
  `album.delete` / `albumUser.delete`), and a teardown already in progress is paused (pair kept, not synced, reported as
  skipped) until it is turned on again.
- Ambiguous names (several albums with the same name on one side, or the name already used by a paired or viewer-only album) are
  skipped and reported in `/status`, the logs and `once`/`validate` output. Albums without a name are skipped.
- An album that is no longer selected (marker removed from both sides, or no longer editable) **stops being synced**: albums and
  their contents are left as they are and the pair's state is reset. With album info sync on, removing the marker on one side is
  copied to the other side first, so the pair stops one cycle later.
- An album deleted on one side (or no longer accessible) stops the pair, and its counterpart is **never re-created** automatically.
  Create an album with the same name by hand to resume.
- Discovered pairs are named `album-<album id on A>`; use that name with `approve-removals` and `state show|reset`.
- Lost discovery state is safe: albums are re-paired by name and synced with union semantics (nothing is removed). It also forgets
  which albums were created and shared, so an album everyone had already left keeps syncing instead of being torn down; a teardown
  interrupted half-way leaves the counterpart behind (delete it by hand). Without a pair's state nothing is ever trashed.
  With a **dedicated sync user** (it owns no albums except those discovery created) set `SYNC_DISCOVERY_DEDICATED_USER=true`:
  an unpaired album it owns that nobody else is a member of is then never synced and blocks its same-name album on the other
  side, so even without `discovery.json` an album everyone left is never re-paired or re-created (it is reported as skipped;
  delete it or share it again). Albums that are already paired are not affected.

## Requirements

- Immich **>= 3.2 and < 4** (tested with v3.4.1); other major versions are refused at preflight.
- One API key per instance. The user needs the owner or editor role on the synced album.
- API key permissions (preflight reports exactly which are missing; `all` is also accepted):

| Permission | Needed for |
|---|---|
| `user.read` | identify the key's user (always); look up `SYNC_DISCOVERY_SHARE_*` emails |
| `album.read` | read albums (always) |
| `asset.read` | list/probe album assets, read assets (always) |
| `asset.download` | download originals (always) |
| `asset.upload` | upload copies (always) |
| `albumAsset.create` / `albumAsset.delete` | add to / unlink from albums (always) |
| `album.create` | creating missing counterpart albums (`SYNC_DISCOVERY=all` or `marker`) |
| `asset.update` | metadata sync, live-photo linking (`SYNC_METADATA`, live photos) |
| `album.update` | album name/description sync (`SYNC_ALBUM_INFO`) |
| `asset.delete` | restoring from trash (`SYNC_RESTORE_TRASHED`); trashing on teardown (`SYNC_DISCOVERY_TEARDOWN_TRASH`) |
| `album.delete` / `albumUser.delete` | teardown when everyone left a shared album: delete it, leave its counterpart (`SYNC_DISCOVERY_SHARE_*` set, `SYNC_DISCOVERY_TEARDOWN` not `false`) |

**Upgrading with `SYNC_DISCOVERY_SHARE_*` set:** the key now also needs `album.delete` and `albumUser.delete`; preflight fails
with the missing list until you add them.

The authoritative list is computed from your options at start-up; the table above is a guide.

## Quick start (Docker)

```sh
make image                                   # builds immich-ferry:dev
cp examples/docker/sync.env.example sync.env # edit URLs, API keys, albums
docker run -d --name immich-ferry --restart unless-stopped --stop-timeout 60 \
  -v immich-ferry-data:/data --env-file sync.env immich-ferry:dev
docker logs -f immich-ferry
```

See [`examples/docker/`](examples/docker) for a hardened run script, compose file and multi-pair YAML config.
Start with `SYNC_DRY_RUN=true` (or `immich-ferry validate`) to see what would happen.

## Kubernetes (Helm)

The Helm chart is maintained in [RenzoDemuylder/helm-charts](https://github.com/RenzoDemuylder/helm-charts/tree/main/charts/immich-ferry)
(`charts/immich-ferry`, published to `oci://ghcr.io/renzodemuylder/charts/immich-ferry`). Its README documents every value.

```sh
kubectl create secret generic immich-ferry-keys \
  --from-literal=immich-a-api-key=... --from-literal=immich-b-api-key=...
helm install immich-ferry oci://ghcr.io/renzodemuylder/charts/immich-ferry \
  --set immich.a.url=https://photos-a.example.com \
  --set immich.b.url=https://photos-b.example.com \
  --set existingSecret=immich-ferry-keys \
  --set album.a.id=<album-uuid-on-a> --set album.b.id=<album-uuid-on-b>
```

Several pairs: set `pairs` (a list of `{name, a: {albumId}, b: {albumId}}`) instead of `album`; or set `sync.discovery`
(`all` or `marker`). The chart always runs one replica with a `Recreate` strategy, mounts API keys as files, uses a PVC for
`/data`, a size-limited emptyDir for `/tmp` (`SYNC_TMP_BUDGET` = 80% of `tmp.sizeLimit`), sets `GOMEMLIMIT` to 90% of the memory
limit, and a restricted security context (non-root, read-only root filesystem, all capabilities dropped). `helm test` checks
`/readyz`, `/healthz` and `/metrics` through the Service.

## Configuration reference

Configuration comes from environment variables or a YAML file (`SYNC_CONFIG_FILE`, for multiple pairs between the same two
instances); environment variables override the file. Secrets can be supplied as `*_API_KEY_FILE`.

| Variable | Default | Description |
|---|---|---|
| `IMMICH_A_URL`, `IMMICH_B_URL` | required | instance URL, with or without `/api` |
| `IMMICH_{A,B}_API_KEY` / `_API_KEY_FILE` | required | API key or path to a file holding it |
| `IMMICH_{A,B}_INSECURE_TLS` | `false` | skip TLS verification |
| `SYNC_{A,B}_ALBUM_ID` | - | album UUID per side (the last part of the album URL); required unless discovery is on |
| `SYNC_DISCOVERY` | `off` | `off`, `all` or `marker` (see [Choosing albums](#choosing-albums)); not combinable with configured pairs |
| `SYNC_DISCOVERY_MARKER` | `🔁` | text an album description must contain in `marker` mode |
| `SYNC_DISCOVERY_SHARE_A`, `SYNC_DISCOVERY_SHARE_B` | - | comma-separated emails or user ids on that side to share albums created by discovery with |
| `SYNC_DISCOVERY_SHARE_ROLE` | `editor` | `editor` or `viewer`: role of those users on created albums |
| `SYNC_DISCOVERY_DEDICATED_USER` | `false` | the sync user owns only albums discovery created: never sync or pair an unpaired album it owns without other members (needs `SYNC_DISCOVERY_SHARE_*`) |
| `SYNC_DISCOVERY_TEARDOWN` | `true` | tear down a pair when everyone left its created, shared album (see above); `false` keeps syncing it and pauses a teardown in progress |
| `SYNC_DISCOVERY_TEARDOWN_TRASH` | `false` | when everyone left a created album, also trash the photos the sync user uploaded into it (see caveat above; needs `SYNC_DISCOVERY_SHARE_*` and the teardown) |
| `SYNC_PAIR_NAME` | default | name of the single pair (`^[a-z0-9][a-z0-9-]{0,62}$`) |
| `SYNC_CONFIG_FILE` | - | YAML file with `pairs` (see below) |
| `SYNC_INTERVAL` | `15m` | time between cycles |
| `SYNC_CONCURRENCY` | `4` | parallel copies |
| `SYNC_STATE_DIR` | `/data` | state directory (holds a `.lock` file) |
| `SYNC_TMP_DIR` | OS temp | scratch dir for in-flight transfers |
| `SYNC_TMP_BUDGET` | `0` | bytes of scratch usable at once (0 = unlimited) |
| `SYNC_DRY_RUN` | `false` | plan and log only; no mutations, no state save |
| `SYNC_PROPAGATE_REMOVALS` | `true` | unlink removed items on the other side |
| `SYNC_MAX_REMOVAL_RATIO` | `0.2` | removal guard: fraction of tracked items |
| `SYNC_MAX_REMOVALS_MIN` | `10` | removal guard: minimum allowance (must be >= 1) |
| `SYNC_CONFLICT_WINNER` | `a` | `a` or `b`, wins when both sides changed a field |
| `SYNC_METADATA` | `true` | sync asset metadata |
| `SYNC_ALBUM_INFO` | `true` | sync album name/description |
| `SYNC_RESTORE_TRASHED` | `true` | restore a trashed duplicate on the target |
| `SYNC_COPY_BUDGET` | `20m` | max time spent on copies per cycle (must be > 0) |
| `SYNC_PENDING_META_TIMEOUT` | `1h` | how long a new upload's source EXIF wins (must be > 0) |
| `HTTP_ADDR` | `:8080` | listen address for health/metrics; must be `host:port` (invalid values exit 2). The `healthcheck` command probes it (wildcard host -> loopback) and falls back to the config file's `httpAddr`. The image sets no `HTTP_ADDR`/`SYNC_STATE_DIR` env, so the YAML values apply |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | logging (`json` or `text`) |
| `SSL_CERT_FILE` | - | custom CA bundle |

YAML file layout (all keys optional except `pairs` when discovery is off; see `examples/docker/config.example.yaml`):

```yaml
instances: {a: {url, apiKey, apiKeyFile, insecureTLS}, b: {url, apiKey, apiKeyFile, insecureTLS}}
sync: {interval, concurrency, tmpDir, tmpBudget, dryRun, propagateRemovals, maxRemovalRatio, maxRemovalsMin, conflictWinner,
       metadata, albumInfo, restoreTrashed, copyBudget, pendingMetaTimeout, discovery, discoveryMarker,
       discoveryShare: {a: [emails or ids], b: [...], role, teardown, trashOnTeardown, dedicatedUser}}
pairs: [{name, a: {albumId}, b: {albumId}}]
stateDir: /data
httpAddr: ":8080"
log: {level, format}
```

Commands: `run` (default), `once [--strict]`, `validate`, `healthcheck`, `version`, `approve-removals --pair`,
`state show|reset --pair`. `once` exits 0 on ok/partial (1 with `--strict`), 1 on failure, 2 on configuration errors.

## Operations

- **Endpoints**: `/healthz` (liveness: a heartbeat refreshed by progress events and by the Immich client while it legitimately waits, plus a check that the loop is not idle for longer than its planned sleep + 10m), `/readyz` (configuration and HTTP only, so an Immich outage does not make the pod
  unready), `/status`, `/metrics`.
- **Metrics**: `immich_ferry_runs_total{pair,result}`, `_last_success_timestamp_seconds` (ok runs, and partial runs without item errors), `_last_run_timestamp_seconds{pair}` (any completed run), `_run_duration_seconds`,
  `_assets_copied_total{pair,direction}`, `_bytes_total`, `_unlinked_total`, `_metadata_updates_total`, `_skipped_total{pair,reason}`,
  `_item_errors_total{pair,kind}`, `_album_assets{pair,side}`, `_pending`, `_guard_tripped{pair,side}`,
  `_snapshot_consistent{pair,side}`, `_preflight_ok{side}`; with discovery `_discovery_ok`, `_discovered_pairs`,
  `_discovery_problems` (selected albums that could not be paired or created), `_albums_created_total{side}`,
  `_pairs_torn_down_total{side}` and `_assets_trashed_total{side}` (teardown). Series of a pair that
  stops being synced are removed. The per-side album gauges are only updated by runs whose snapshot phase completed; a failed run keeps the previous values.
- **Logs** use structured keys `pair, side, key, asset_id, op, kind, phase`.
- **Mass-removal guard**: when removals on a side exceed `max(SYNC_MAX_REMOVALS_MIN, ratio x tracked items)` all removals for the pair are
  skipped and the run reports an error. Inspect, then `immich-ferry approve-removals --pair <name>` (one-shot) or run
  `once --allow-mass-removal`.
  Kubernetes: `kubectl exec deploy/<name> -- /immich-ferry approve-removals --pair <name>`; Docker: `docker exec immich-ferry /immich-ferry ...`.
- **State**: `immich-ferry state show --pair <name>`; `state reset --pair <name> --yes` forgets removal tracking (the next run re-bootstraps and
  never removes). Back up `/data` (about 20 MB for large albums); a lost state is safe, a rolled-back one is safe too.
  A stored album that has been deleted is a pair error; the album is never recreated.
- **Single writer**: a `.lock` file (flock) in the state dir prevents two processes sharing it. flock may not work on NFS; do not share
  `/data` between replicas or hosts.
- **Upgrading**: pull the new image and restart (Helm: `helm upgrade`; pod restarts with `Recreate`). SIGTERM finishes with a final
  state save within the 60s grace period; a second signal exits immediately.
- **Scratch space**: large videos are streamed to disk. Size `/tmp` (`tmp.sizeLimit`) above your largest file; the byte budget (~80%)
  makes transfers wait instead of filling the disk, and a file larger than the budget runs alone.
- **Reverse proxies** in front of Immich must allow large request bodies (uploads up to several GB), not buffer them fully, and use
  long timeouts. A 413 is recorded as a permanent `too_large` skip.
- **Redirects are never followed** (they would forward the API key and downgrade POST/PUT to GET): if a base URL redirects
  (http to https, trailing host change), the run fails with "base URL redirects to ...; configure the final URL". API keys must not
  contain whitespace or control characters (a two-line key file is rejected at startup).
- **Slow or trickling transfers** are cancelled and retried when fewer than 64 KiB move per stall window (2m).
- Chart: `tmp.medium=Memory` requires `tmp.sizeLimit` below `resources.limits.memory`; `GOMEMLIMIT` is set to 90% of the memory limit.
- Use `validate` (read-only) to check configuration and connectivity, and `SYNC_DRY_RUN=true` to preview a cycle.

## Safety and what can go wrong

| Risk | Mitigation |
|---|---|
| Wrong unlink (paging gaps, concurrent trash, hidden/offline items) | per-item removal probe before any unlink |
| Mass unlink (revoked key, emptied album) | ratio guard plus explicit approval; assets are never deleted |
| Lost, corrupt or rolled-back state | empty state never removes; corrupt state is moved aside; URL changes do not reset |
| Metadata overwrite / ping-pong | per-side baselines bound to asset id, read-back after writes, tz/float normalization |
| Trashed duplicate on target | restore and verify (not during bootstrap), otherwise block |
| Live photo orphans | motion uploaded hidden first; never link a visible motion asset |
| Truncated download | SHA1 verified before upload |
| Huge videos | streamed to disk with a byte budget; fail fast if larger than limits |
| Two processes on one state | flock; Helm uses 1 replica and `Recreate` |
| SIGTERM mid-transfer | cancellation is not a failure; final state save |
| Immich down | `/readyz` ignores Immich; `immich_ferry_preflight_ok` metric; transient errors retry with backoff |
| Missing permissions / wrong version | preflight lists missing permissions; major version gate |
| Discovery pairs the wrong albums | exact-name match only; ambiguous names are skipped, never guessed |
| Duplicate albums from discovery | album creation is not retried; an album already there is adopted by name; a listing that misses a paired album is double-checked by id |
| Deleted album re-created | a deleted counterpart is remembered and never re-created automatically |
| Teardown on a false "everyone left" | only albums discovery created and shared; members confirmed by id; ambiguous → kept, not synced |
| Teardown trashes someone's photo | off by default; only photos this tool uploaded (pair state), in no album the sync user sees; trash, never permanent |

## Limitations

Not synced: stacks, tags, people, non-destructive edits, album sharing/users/thumbnail, and album or asset deletion (except the
teardown of shared albums everyone left).
Clearing a location is not propagated. Locked assets are invisible to API keys. Immich 4.x is not supported.

## Development

```sh
make build test vet lint      # go build / go test -race / go vet / gofmt check
make image                    # docker build (VERSION=... to tag)
make image-multiarch          # amd64+arm64 (needs a docker-container buildx builder; PUSH=1 to push)
make chart-lint               # helm lint --strict of the chart in ../helm-charts (HELM_CHARTS=...) with every ci values file
make e2e-up e2e-down e2e      # e2e against two real Immich v3.4.1 stacks (docker compose)
make e2e-kind                 # optional Kubernetes e2e in a dedicated kind cluster
```

Design details: [`docs/DESIGN.md`](docs/DESIGN.md). Per-cycle flow diagrams: [`docs/SYNC_FLOW.md`](docs/SYNC_FLOW.md).
