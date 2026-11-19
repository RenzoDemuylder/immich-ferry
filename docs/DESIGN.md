# immich-ferry — DESIGN (source of truth for all implementers)

## Context
User wants a container that keeps an album in sync between two Immich instances using only the REST API, running
unattended in the background (Kubernetes pod; plain `docker run` must also work). Implementation is done by **sonnet subagents**.

User decisions: **bidirectional**; **Go**; removal = **unlink from the other album only, never delete assets**; sync
**asset edits (description/date/location/rating), archive, album name/description, live photos**; favorites are per user
and **not synced** (removed 2026-11; old state files may still carry a `favorite` baseline, which is ignored);
**Helm chart + plain docker**; e2e via **docker compose with two real Immich instances**; **optional** kind e2e.

Target **Immich v3.4.1** (latest stable, 2026-11-11). Minimum **v3.2.0** (structured search). Preflight refuses major ≠ 3.

## Verified API facts (v3.4.1) — ground truth for implementation
- Auth `x-api-key`; paths under `/api`. Errors: Zod `{"message","errors":[{path,message}]}`, header `X-Correlation-ID`.
  Access problems come back as **400 "Not found or no X access"** (map to NotFound), not 404.
- `GET /server/version` → `{major,minor,patch}` (public). `GET /api-keys/me` (no perm) → `{permissions[]}` (may be `["all"]`). `GET /users/me` (`user.read`) → `{id}`.
- `GET /albums/{id}` (v3: **no assets**): `albumName, description (""=null), assetCount (non-trashed timeline+archive), albumUsers[{user{id},role}], updatedAt`.
  Album-asset **add bumps updatedAt, remove does not**. Add has **no visibility check** (hidden/locked can be added).
- **List album assets** — structured search only (legacy flat `withExif` INNER-JOINs exif and silently drops assets):
  `POST /search/metadata {"filter":{"albumIds":{"any":[id]},"trashedAt":{"eq":null},"visibility":{"in":["archive","timeline"]}},"orderBy":{"field":"fileCreatedAt","direction":"asc"},"withExif":true,"size":1000,"cursor":C}`
  → `{assets:{items,nextCursor}}`. Offset cursor, `id` tie-break. Album-confined → all owners' assets. Never mix with flat fields (400).
  **Probe**: same with `filter:{albumIds:{any:[id]}, checksum:{in:[...]}}` and NO trash/visibility filter (returns trashed+hidden, never locked).
- AssetResponseDto used fields: `id, ownerId, checksum (base64 SHA1 of original; PATH hash if libraryId≠null), libraryId, originalFileName, fileCreatedAt, fileModifiedAt, type, duration (int ms|null), livePhotoVideoId, isFavorite (false unless caller owns), visibility, isTrashed, isOffline, exifInfo{description, dateTimeOriginal (UTC), timeZone, latitude, longitude, rating, fileSizeInByte}`. Rating 0 never appears (stored null).
- Download `GET /assets/{id}/original` (`asset.download`) → unedited original, streamed; album access covers the still's `livePhotoVideoId`.
- Upload `POST /assets` multipart: text fields first (`fileCreatedAt`, `fileModifiedAt` RFC3339 **with offset**, `filename`, `duration` int ms, `isFavorite` "true"/"false", `visibility`, `livePhotoVideoId`), then `assetData` (part filename = original name). No deviceId/deviceAssetId (removed v3).
  201 `{id,status:"created"}` / 200 `{id,status:"duplicate"}`. Duplicate = same owner+checksum (non-library) **incl. trashed**; request fields are NOT applied on duplicate. Do **not** send `x-immich-checksum` (early 200 → broken pipe).
- `POST /assets/bulk-upload-check {assets:[{id,checksum}]}` → `{results:[{id,action:accept|reject,reason,assetId,isTrashed}]}` (no livePhotoVideoId/visibility → GET the asset).
- Live photo: upload motion first with `visibility=hidden`, then still with `livePhotoVideoId`; or `PUT /assets/{still} {livePhotoVideoId}` (hides motion, does not remove from albums). Server **auto-link** (ContentIdentifier) hides motion **and removes it from all albums**.
- Albums: `GET /albums` (no filters) → owned + shared-with-me albums, `albumUsers` incl. owner (role owner) and my role;
  `POST /albums {albumName,description,albumUsers?:[{userId,role editor|viewer}]}` (only `album.create` needed; own id ignored;
  unknown user → 400 "Invalid user", other role or repeated user → 500; nothing created on error; no API transfers ownership);
  `GET /users` (`user.read`) → `[{id,email,name}]` of all users (default server setting); `PUT|DELETE /albums/{id}/assets {ids}` → `[{id,success,error}]` (`duplicate`/`not_found` = ok); `PATCH /albums/{id} {albumName,description}`.
- `PUT /assets/{id}` (owner only) `{description, dateTimeOriginal, latitude+longitude together, rating -1|1..5|null, isFavorite, visibility, livePhotoVideoId}` → AssetResponseDto (read-back). Date write turns IANA tz into `UTC+N`; send only changed fields. Moving to `locked` removes from all albums.
- `POST /trash/restore/assets {ids}` (`asset.delete`); count unreliable → re-GET.
- Teardown (verified by `TestSmokeTeardownFacts`): `DELETE /albums/{id}/user/me` (`albumUser.delete`) leaves an album shared
  with the caller (204); the owner cannot (400 "Cannot remove the last album owner"); again → 400 "Album not shared with user";
  afterwards the album is 400 "Not found…". `DELETE /albums/{id}` (`album.delete`, owner) keeps the assets; a gone album → 400
  "Not found or no album.delete access". `DELETE /assets {ids, force:false}` (`asset.delete`) trashes; album search no longer
  lists the asset. `GET /albums?assetId=` lists only the caller's albums (owned/shared) holding it. A **member can add the
  owner's assets to an album of their own** (access via the shared album), keeps that access after leaving, and the owner's
  `?assetId` does not list that album.
- No server rate limit / upload size limit; JSON body ≤10MB → chunk id lists ≤1000.

## Architecture (Go 1.27, module `immich-ferry`; deps only prometheus/client_golang + yaml.v3)
```
cmd/immich-ferry/            run | once [--strict] | validate | healthcheck | version | approve-removals --pair | state show|reset --pair
internal/immich/            client, DTOs, retry/backoff, typed error kinds, phase-aware timeouts
internal/immich/fakeimmich/ in-memory fake Immich (httptest) with fidelity rules + fault hooks
internal/state/             per-pair JSON state, atomic save, <dir>/.lock flock, markers
internal/engine/planner/    PURE: truth table, guards, blocked semantics, per-side metadata merge, normalization
internal/engine/            preflight, snapshot, removal probe, transfer, live photo, executor, budgets
internal/config/            env + YAML + *_FILE secrets → engine.Options/Pair
internal/daemon/            loop, signals, /healthz /readyz /metrics /status
test/e2e/                   compose.yaml, testutil, scenarios (tag e2e), kind test (tag e2e_kind)
examples/docker/  Dockerfile  Makefile  README.md  docs/DESIGN.md   (Helm chart: github.com/RenzoDemuylder/helm-charts, charts/immich-ferry)
```
Go contracts (types/signatures) are pinned in the **Appendix** and copied into `docs/DESIGN.md` as compile-able stubs before parallel work.

## Sync algorithm (per pair, per cycle)
Phase order: **preflight → snapshot → plan → removals (probe-confirmed) → metadata → album info → copies (time budget)**.
Fast phases first so writes act on fresh snapshots; copies capped by `CopyPhaseBudget` (20m), remainder next cycle.
Flow diagrams of every branch per sync period: [SYNC_FLOW.md](SYNC_FLOW.md) (illustrative; this file wins on conflict).

1. **Preflight**: version ≥3.2 <4; key perms ⊇ `RequiredPermissions(opts)` (`all` = superset; perms derived from options); user id.
   Albums: configured id (never created by RunPair). Missing id → config error. Role owner/editor required.
   Album gone → **pair error, never recreate**. Duplicate-album-across-pairs check after resolution.
   With discovery (`SYNC_DISCOVERY` all|marker) the pairs come from `Engine.Pairs` (below) before the pair runs.
2. **Snapshot** both sides fully (compact decode). Consistency (album count before/after vs items) → metric only.
3. **Key** = checksum; external-library assets → SHA1 of content (hash cache in state by assetId+fileModifiedAt; record free when copying).
   Group same-key assets per side; primary deterministic = owned-by-me first, then lowest id. Motion parts never are album items.
4. **Truth table** (S = keys in both albums after last success, `Item.InBoth`):
   | inA | inB | inS | action |
   |---|---|---|---|
   | 1 | 1 | – | paired → metadata merge; S+=key |
   | 1 | 0 | 0 | copy A→B |
   | 0 | 1 | 0 | copy B→A |
   | 1 | 0 | 1 | candidate removal on B → probe B → unlink from A |
   | 0 | 1 | 1 | candidate removal on A → probe A → unlink from B |
   | 0 | 0 | 1 | drop + GC failures/pending/baselines |
   Empty S (first run/lost/reset) never removes. `Bootstrap` flag until first fully-successful cycle: no-baseline merge rules, **no trash restore**.
5. **Removal probe** (authoritative, per candidate side): not member or member+trashed (not offline) → propagate unlink;
   member+hidden → block `server_hidden` (auto-link), never propagate; member+offline → skip cycle; still visible member → snapshot glitch, skip.
6. **Guards**: removals on a side > max(`MaxRemovalsMin`=10, `MaxRemovalRatio`=0.2 × S size) → skip all removals for pair, `GuardTripped`, error
   until `immich-ferry approve-removals --pair X` marker (one-shot) or `--allow-mass-removal` on `once`.
   Guard tripped / `PropagateRemovals=false` → S untouched (removals ignored, never resurrected).
   **Blocked** keys stay in S, planner does nothing for them; block clears when the key's row changes or after `BlockedRecheck` (24h).
   Unlink `no_permission` → block. Per-key failure backoff (15m→24h exp) in state; **ctx cancellation never recorded as failure**.
   Quota exceeded → stop uploads to that side this cycle; N consecutive transient errors → abort pair; 413 → `too_large` permanent backoff.
7. **Copy s→t**: write-ahead **Intent** (key, to, source ref+meta) persisted per batch before uploads.
   a. `bulk-upload-check` (chunks 1000). Existing → `GET /assets/{id}`: visibility ∉{timeline,archive} → block `target_hidden`, no album add;
      trashed → restore (not during bootstrap) + re-GET verify, else block `target_trashed`.
   b. Accept → acquire tmp byte budget (`TmpBudgetBytes` by `fileSizeInByte`; larger-than-budget runs alone; > hard limit fails fast) →
      stream download to temp file + SHA1 → verify == source checksum (non-library; retry 3) → upload from file (Content-Length).
      Fields: `fileCreatedAt, fileModifiedAt, filename, duration, visibility` (never `isFavorite`: favorites are per user).
   c. Live photo: GET target still (if exists): already linked → done. Else copy motion (hidden; restore if trashed); only link a target motion
      that is hidden or just created by us — never a timeline/archive one (copy still alone + warn). Upload still with `livePhotoVideoId` or `PUT` link.
      Paired still missing its motion on target → "repair link" in metadata phase. Android motion photos: still only (target re-extracts).
   d. Album add in batches (flush every 100), never same key concurrently, worker pool `Concurrency`=4.
   e. `status:"created"` (or Intent match) → `Pending` (source-wins for EXIF fields until target extracted: `exifInfo.dateTimeOriginal≠nil`
      or `PendingMetaTimeout` 1h). Visibility set at upload → baseline immediately. Reused duplicates → normal merge.
8. **Metadata merge** (description, date+offset, lat/lon, rating, visibility∈{timeline,archive}; never favorite) with **per-side last-observed
   baselines bound to asset id** (`Meta[s]`, `MetaID[s]`): side changed iff cur≠base (normalized). One side changed → re-GET target
   (skip if changed since snapshot = `stale`), PUT only changed fields, `base[target]` = PUT read-back (no ping-pong by construction).
   Both changed → `ConflictWinner` (A). **No baseline** (new pairing / id changed): description/rating/location fill empty side only;
   date/visibility no write, just record baselines + log divergence. Not owned → skip `not_owner`.
   Normalize: description null==""; date by instant; tz by UTC offset at that instant (IANA via embedded `time/tzdata`, `UTC±N`, null=UTC);
   lat/lon ε 1e-6. Clearing location unsupported → skip+warn.
9. **Album info merge** (name, description): same per-side baseline logic; re-GET album before PATCH.
10. **State**: `Save` atomic (tmp+fsync+rename+dir fsync), **no ctx**; checkpoint every 100 actions or 30s (whichever later) + end.
    Fingerprint = user ids + album ids (**no URLs**). Corrupt → moved aside, fresh. Dry-run: no mutations, no save.
11. **Album discovery** (`Engine.Pairs`, `internal/engine/discover.go`), once per cycle after preflight, mutually exclusive with
    configured pairs. Selected = role owner/editor AND (mode `all` OR description contains `DiscoveryMarker` on either side).
    `GET /albums` (no filters) both sides; error → discovery fails, no pair runs this cycle.
    - Discovery state `<dir>/discovery.json`: `{userIds, pairs[{name, albumIds[2], since}], tombstones[{side, albumId, reason}]}`,
      atomic save, bound to user ids (mismatch/corrupt → moved aside, fresh). Pair name `album-<A album id>`.
    - Pinned pair: an album missing from the listing is confirmed with `GET /albums/{id}` (listing glitch never unpairs);
      lookup error → keep pair, skip it this cycle. Gone (not found) or not editable on a side → unpair + tombstone the other
      side's album. Neither side selected → unpair (no tombstone). Unpair = move pair state aside (`Store.Reset`), release album
      claims; albums/assets untouched.
    - Unpinned selected album x (side s): skip if unnamed, or another unpinned editable album on s has the same name. Same-name
      albums on the other side o: exactly one free (unpinned, editable) and none blocked → pair; >1 free or any blocked
      (pinned/viewer) → skip. None → tombstoned x → skip ("counterpart deleted"), else `POST /albums {name, description of x, albumUsers}` on o
      (never retried; an ambiguous failure is adopted by name next cycle) and pair. Pairing clears tombstones of both albums.
    - Share (`DiscoveryShare[o]`, role `DiscoveryShareRole`, default editor): the created album is shared in the same
      `POST /albums` (`albumUsers`), so it never exists unshared. Entries with `@` are emails resolved via `GET /users`
      (case-insensitive, once per pass and side, only when a create is due, also in dry run); others are user ids sent as is
      (unknown → server 400, nothing created). Own user and repeats dropped. Resolution error → album not created, `Errors`
      note, retried next cycle. Only albums discovery creates are shared; existing albums and their members are never changed.
    - Tombstones of albums that are gone (GET not found) are dropped.
    - Dry run / read-only store: no create, no state write, no reset; `WouldCreate` reported. Report (`DiscoveryReport`):
      pairs, created, wouldCreate, unpaired, skipped, errors → `/status`, `once`/`validate` JSON, metrics.
    - Lost discovery state: albums are re-paired by name (same ids → same pair names, pair states reused; otherwise fresh
      bootstrap, union, never removes). A deleted counterpart may then be re-created (tombstone lost).
    - **Teardown** (`internal/engine/teardown.go`, only with `DiscoveryShare` on the created side). `DiscoveredPair.Created
      {side, sharedWith}` is set when discovery creates a shared album, or backfilled for older pairs when the sync user owns
      exactly one album of the pair, on a shared side, and it has members. Trigger: that album has no member besides the sync
      user (only it could remove members, so they left), confirmed with `GET /albums/{id}` (error → keep, not synced, retry).
      Steps, each persisted in `DiscoveredPair.Teardown` (saved before the first step and after each): (1) leave the original
      album X on the other side (gone → done; sync user owns X → cannot leave, `KeptAlbum`: the created album is kept, emptied
      of members, so X never gets a new counterpart); (2) only with `DiscoveryTeardownTrash`: trash (`force:false`) assets of
      the created album that are owned by the sync user, recorded as uploaded by us (`Item.Ours[side]`, set on `created` or our
      own retried duplicate) and listed by `?assetId` in no other album (lookup error → keep); (3) delete the created album;
      (4) unpair (`Store.Reset`) and tombstone X (and the kept album) with the teardown reason. Teardown tombstones also block
      pairing by name (skipped, reported). Errors keep progress and retry next cycle; a pair in teardown is never synced, even
      if someone re-joins. Leave/delete errors are re-checked with a GET (not found = done). Dry run → `WouldTeardown`.
      `DiscoveryTeardownOff` (`SYNC_DISCOVERY_TEARDOWN=false`): the trigger is never checked (no GET), the pair keeps
      syncing, `album.delete`/`albumUser.delete` are not required; a pair with `Teardown` progress is paused (kept, pinned,
      not synced, reported skipped) and resumes when re-enabled. Incompatible with `DiscoveryTeardownTrash` (config error).
      Lost `discovery.json`: re-paired by name and no `Created` (no members to backfill) → keeps syncing, never torn down; an
      interrupted teardown after (1) leaves the created album behind (manual cleanup). Lost pair state → nothing trashed.
      `DiscoveryDedicatedUser` (opt-in; the sync user owns only albums discovery created): an **unpinned** album owned by the
      sync user with no other member, on a shared side, is skipped as a source and counts as blocked for same-name pairing, so
      after a lost `discovery.json` an album everyone left is never re-paired or re-created. Pinned pairs are unaffected.
      Not the default: with a personal account its own unshared albums would stop being discovered.

## What can go wrong → mitigation
| Risk | Mitigation |
|---|---|
| Wrong unlink: offset-paging gaps, concurrent trash/restore, hidden/locked/offline/auto-linked targets | removal probe per candidate; snapshot consistency only a metric |
| Mass unlink (revoked key, emptied/wrong album) | ratio guard + explicit approve marker; never deletes assets |
| Lost/corrupt/rolled-back state | empty S never removes (resurrects at worst); corrupt moved aside; URL change doesn't reset |
| Metadata overwrite with no baseline / re-imported asset / reused duplicate | per-side baselines bound to asset id; fill-empty only; source-wins only for our uploads (Intent) |
| Ping-pong (tz normalization, float, sync's own writes) | per-side last-observed baselines, read-back after PUT, offset-equivalent tz compare |
| Lost update on long cycles | phase order, re-GET before write, copy time budget |
| Trashed duplicate on target | restore+verify (not in bootstrap); else block |
| Live photo orphans / album removal by auto-link | motion uploaded hidden first, never add motion to album, never link visible motion |
| Truncated download | SHA1 verify before upload |
| Huge videos: OOM, emptyDir eviction crash-loop | stream to disk, byte-budget semaphore (~80% of `tmp.sizeLimit`), fail-fast if larger |
| Proxy buffering 4GB upload → stall watchdog loop | stall timeout only while bytes flow; `ResponseWait(size)` after body; 413 permanent |
| Two processes on one state | `<dir>/.lock` flock for process life; Helm replicas 1 + Recreate; NFS caveat doc'd |
| SIGTERM / helm upgrade mid-transfer | ctx cancel ≠ failure, final Save without ctx, grace 60s, 2nd signal exits now |
| Immich down → pod unready → metrics lost | `/readyz` = config+HTTP only; `immich_ferry_preflight_ok{side}` metric; `/healthz` = heartbeat only |
| Long initial sync killed by liveness | heartbeat on progress events; stale iff active phase w/o progress > max(10m, 3×stall) |
| Album renamed (name sync) / deleted | ids persisted after first resolution; deleted → pair error, no recreate |
| Discovery pairs wrong albums / creates duplicates | exact-name match only, ambiguity skipped; create never retried, adopted by name next cycle; listing misses confirmed by GET |
| Created counterpart invisible to the album's people (owned by the sync user) | `DiscoveryShare`: shared in the create request itself; unresolvable email → not created (error, next cycle); existing albums never re-shared |
| Discovered counterpart deleted | unpair + tombstone the survivor: never re-created automatically (until a same-name album appears) |
| Everyone left a shared created album | teardown: leave original, (opt-in) trash own uploads, delete created album, unpair; trigger confirmed by GET; resumable |
| Teardown trashes a photo a member kept | trashing off by default (`SYNC_DISCOVERY_TEARDOWN_TRASH`): a member's own albums are invisible to the sync user; only `Item.Ours` assets in no visible album; trash, never `force` |
| Missing perms / viewer role / v4 drift | preflight with exact missing-perm list; version gate; e2e pinned (`IMMICH_VERSION` overridable) |
| Transient 5xx/429/network | retry exp backoff+jitter (all ops idempotent); 4xx no retry |
| Distroless: no shell for admin ops | `approve-removals`, `state show/reset` subcommands (lock-free markers), `validate` read-only |
| 50k-asset albums memory | compact decode, `GOMEMLIMIT` from memory limit, ~20MB state |
Out of scope (documented): stacks, tags, people, non-destructive edits, favorites (per user), album sharing/users (except sharing
albums discovery creates and leaving/deleting them in a teardown)/thumbnail, album or asset deletion propagation.

## Runtime / packaging contract
- Env (single pair) or YAML `SYNC_CONFIG_FILE` (multiple pairs between the same two instances); env overrides file:
  `IMMICH_{A,B}_URL` (with/without `/api`), `IMMICH_{A,B}_API_KEY` | `_API_KEY_FILE`, `IMMICH_{A,B}_INSECURE_TLS`,
  `SYNC_{A,B}_ALBUM_ID` (UUID, required unless discovery), `SYNC_DISCOVERY` off|all|marker, `SYNC_DISCOVERY_MARKER` 🔁,
  `SYNC_DISCOVERY_SHARE_{A,B}` (comma list of emails/user ids; requires discovery), `SYNC_DISCOVERY_SHARE_ROLE` editor|viewer,
  `SYNC_DISCOVERY_TEARDOWN` true (YAML `discoveryShare.teardown`),
  `SYNC_DISCOVERY_TEARDOWN_TRASH` false (requires a share list and the teardown; YAML `discoveryShare.trashOnTeardown`),
  `SYNC_DISCOVERY_DEDICATED_USER` false (requires a share list; YAML `discoveryShare.dedicatedUser`),
  `SYNC_PAIR_NAME` (default), `SYNC_INTERVAL` 15m, `SYNC_CONCURRENCY` 4,
  `SYNC_STATE_DIR` /data, `SYNC_TMP_DIR` (os temp), `SYNC_TMP_BUDGET` (0=unlimited), `SYNC_DRY_RUN`, `SYNC_PROPAGATE_REMOVALS` true,
  `SYNC_MAX_REMOVAL_RATIO` 0.2, `SYNC_MAX_REMOVALS_MIN` 10, `SYNC_CONFLICT_WINNER` a, `SYNC_METADATA` true, `SYNC_ALBUM_INFO` true,
  `SYNC_RESTORE_TRASHED` true, `SYNC_COPY_BUDGET` 20m, `SYNC_PENDING_META_TIMEOUT` 1h,
  `HTTP_ADDR` :8080, `LOG_LEVEL` info, `LOG_FORMAT` json. TLS CA via standard `SSL_CERT_FILE`.
- `once` exit: 0 ok/partial (1 with `--strict`), 1 failed, 2 config. `healthcheck` derives port from `HTTP_ADDR`.
- Metrics: `immich_ferry_runs_total{pair,result}`, `_last_success_timestamp_seconds{pair}`, `_run_duration_seconds{pair}`,
  `_assets_copied_total{pair,direction}`, `_bytes_total{pair,direction}`, `_unlinked_total{pair,direction}`, `_metadata_updates_total{pair,direction}`,
  `_skipped_total{pair,reason}`, `_item_errors_total{pair,kind}`, `_album_assets{pair,side}`, `_pending{pair}`, `_guard_tripped{pair,side}`,
  `_snapshot_consistent{pair,side}`, `_preflight_ok{side}`, discovery: `_discovery_ok`, `_discovered_pairs`, `_discovery_problems`,
  `_albums_created_total{side}`, `_pairs_torn_down_total{side}`, `_assets_trashed_total{side}`; a pair that stops being synced has its series deleted. slog keys: `pair, side, key, asset_id, op, kind, phase`.
- Image: multi-stage `golang:1.27` builder (`--platform=$BUILDPLATFORM`, TARGETOS/TARGETARCH, CGO off, `-trimpath`, version ldflags) →
  `gcr.io/distroless/static-debian12:nonroot` (verify tag); `/data` created in builder, `COPY --chown=65532:65532`; `VOLUME /data`;
  `EXPOSE 8080`; `HEALTHCHECK ["/immich-ferry","healthcheck"]`; `ENTRYPOINT ["/immich-ferry"]`, `CMD ["run"]`.
- Helm chart (moved to `github.com/RenzoDemuylder/helm-charts`, `charts/immich-ferry`; its values follow that repo's conventions, e.g. `immich.{a,b}.apiKey`, `extraEnvVars`, `metrics.serviceMonitor.*`): Deployment (replicas 1 fixed, Recreate, `terminationGracePeriodSeconds: 60`), PVC (`persistence.*`, `existingClaim`,
  default local-path-friendly), Secret or `existingSecret`, ConfigMap (pairs YAML, album IDs recommended), emptyDir `/tmp` (`tmp.sizeLimit`,
  `SYNC_TMP_BUDGET` ≈80%), `GOMEMLIMIT` = 90% of the memory limit, Service :8080, optional ServiceMonitor, probes (startup+liveness /healthz,
  readiness /readyz), restricted securityContext (nonroot, RO rootfs, drop ALL, seccomp RuntimeDefault, fsGroup 65532), `extraEnv/extraVolumes`, NOTES.txt.
- Plain docker: `examples/docker/` — `docker run -d --restart unless-stopped --stop-timeout 60 -v immich-ferry-data:/data -e ...` + compose (`stop_grace_period: 60s`).

## Tests
- **Unit** (`go test -race ./...`): planner truth table + probe outcomes + guards + blocked + bootstrap; MergeMeta per field, conflicts,
  no-baseline rules, id rebinding, tz offset equivalence; state (atomic save, corrupt, lock, markers, fingerprint); client (request shapes,
  multipart order, cursor paging, retries, KindOf incl. 400-not-found, stall/response-wait) vs httptest; config (env/_FILE/YAML/validation);
  daemon (endpoints, heartbeat, shutdown, exit codes); **engine scenarios vs fakeimmich**: initial union, bidirectional add, removal,
  trash, trashed-dup restore (+bootstrap no-restore), live photo, auto-link-hidden no propagate, hidden/locked duplicate block,
  shared-album grouping, external library hashing, backoff, quota, guard + approve, concurrent-mutation during paging (`OnSearchPage`),
  SIGTERM mid-copy (no failure recorded), idempotency (loop until Pending==0, then next run 0 mutations).
- **fakeimmich fidelity rules** (pinned in DESIGN.md): per-owner+checksum dedupe incl. trashed; album add bumps updatedAt, remove doesn't;
  add without visibility check; editor removing others' asset → no_permission; PUT link hides motion; `Extract(id)` fills exif + auto-links
  (+removeFromAllAlbums); 400 "Not found or no … access"; offset cursor ordered fileCreatedAt,id; reject flat+structured mix;
  `Inject(Fault)`, `OnSearchPage`, `Mutations()`.
- **E2E** (`make e2e`, tag `e2e`, engine in-process with real client; each scenario own albums/state dir/unique media):
  `test/e2e/compose.yaml` two stacks (immich-server v3.4.1, vectorchord postgres pinned digest w/ e2e flags, valkey 9; ML off; explicit
  `DB_HOSTNAME/REDIS_HOSTNAME`), ports `127.0.0.1:22283/22284` (overridable), healthcheck `start_period 180s`, `compose pull` step,
  `up --wait --wait-timeout 600`, `go test -count=1 -timeout 45m`, `down -v`.
  Testutil: admin-sign-up/login, API key with **exactly `RequiredPermissions`** (proves docs), second user for shared album; JPEGs with
  EXIF via Go APP1 writer; tiny committed MP4/MOV fixtures made unique by appended ISO-BMFF `free` box (no host ffmpeg at test time);
  per-asset `Eventually` predicates (exif set, link present, motion hidden) + queues idle on 2 polls.
  Scenarios: initial sync (into an empty B album; photo/video/live photo linked+hidden; filename/dates/checksum preserved); add B→A;
  remove A→unlinked on B (asset still exists); trash on B→unlinked on A; pre-existing B asset reused (no upload) + its description kept;
  trashed duplicate restored; metadata edits both directions + conflict winner; album rename/description both directions;
  idempotent re-run; mass-removal guard + approve; dry-run no changes; shared album with another owner's asset; owner unlinks other
  user's asset with minimal key; service account as editor of a main user's album on both sides (copies owned by the service
  account, metadata/album info/removals both ways, nothing trashed, members unchanged); service account viewer/demoted/revoked →
  ErrConfig with zero mutations, re-share as editor resumes without spurious unlinks; unlinked still+MOV on A auto-linked by B →
  no wrong unlink.
  **Daemon container test**: build image, compose profile `daemon` (interval 5s, named volume, `stop_grace_period 60s`), assert sync,
  `/healthz /readyz /metrics`, `docker stop` idle and mid slow transfer → exit 0, state persisted, restart → no-op cycle.
- **Optional kind** (`make e2e-kind`, tag `e2e_kind`): dedicated cluster `immich-ferry-e2e` (never touch `chart-testing`/`loupe-e2e`),
  `kind load` image (non-latest tag, IfNotPresent), connect Immich containers to `kind` network (container IPs), `helm install --wait --timeout 5m`,
  assert sync, delete pod → PVC state kept, `kind delete cluster` (`KEEP_CLUSTER=1` keeps).
- **Chart**: tested in the helm-charts repo (`make test`: lint, helm-unittest, kubeconform, policies; `make e2e`: kind install + `helm test`); `make chart-lint` here lints it.

## Execution plan (sonnet subagents, `model: sonnet`; each gets docs/DESIGN.md + owned paths; may not change DESIGN semantics)
0. **Me**: `git init` (no commits unless asked), `.gitignore`, `docs/DESIGN.md` (this plan + appendix contracts + fakeimmich rules).
1. **W0 scaffold** (sequential): go.mod (`go 1.27`, deps prometheus + yaml.v3 pinned via blank imports), compile-able stubs for every
   contract (`panic("unimplemented")`), stub `main`, Makefile with all target names. Gate: `go build ./... && go vet ./...`. Sole go.mod owner; later agents stdlib-only (report if a dep is needed).
2. **Wave B (parallel, disjoint paths)**: **W1a** `internal/immich` + `fakeimmich` + tests · **W1b** `test/e2e/compose.yaml` + `testutil` +
   fixtures + bootstrap smoke on real v3.4.1 + Makefile e2e-up/down · **W1c** Dockerfile, .dockerignore, chart, examples, README deploy
   sections (helm lint/template, `docker build`) · **W2a** `internal/engine/planner` + `internal/state` + exhaustive table tests ·
   **W2b** `internal/config` + `internal/daemon` + `cmd/immich-ferry` + tests.
3. **Wave C**: **W2c** `internal/engine` IO (preflight, snapshot, probe, transfer, live photo, executor, budgets) + fakeimmich scenario tests.
4. **Wave D**: **W3** e2e scenarios + daemon container test + kind test; runs them, fixes bugs anywhere, reports any semantic deviation separately.
5. **Wave E**: independent reviewer (code vs DESIGN.md, correctness bugs) → sonnet fix agent → rerun all unit + e2e.
6. **Me**: final verification + summary to user.

## Verification
- `gofmt -l . && go vet ./... && go test -race ./...`
- `docker build -t immich-ferry:dev .`; `docker buildx build --platform linux/amd64,linux/arm64 .` smoke
- `make e2e` — all scenarios + daemon container test green
- `make chart-lint` (chart in `../helm-charts/charts/immich-ferry`)
- Optional `make e2e-kind`
- Manual: `make e2e-up` prints URLs/admin creds → browse `http://127.0.0.1:22283` / `22284`, run `make demo` to see album synced.

---
## Appendix — pinned Go contracts (copied into docs/DESIGN.md; W0 creates as stubs)

```go
// internal/immich
type Visibility string // timeline|archive|hidden|locked
type Version struct{ Major, Minor, Patch int }
type AlbumUser struct{ UserID, Role string }
type Album struct{ ID, Name, Description string; AssetCount int; UpdatedAt time.Time; Users []AlbumUser }
type Exif struct{ Description *string; DateTimeOriginal *time.Time; TimeZone *string; Latitude, Longitude *float64; Rating *int; FileSizeInByte *int64 }
type Asset struct{ ID, OwnerID, Checksum string; LibraryID *string; OriginalFileName string; FileCreatedAt, FileModifiedAt time.Time
	Type string; DurationMS *int64; LivePhotoVideoID *string; IsFavorite, IsTrashed, IsOffline bool; Visibility Visibility; Exif *Exif }
type Opt[T any] struct{ Set bool; Val *T } // Set&&Val==nil → JSON null; !Set → omitted
type AssetUpdate struct{ Description, DateTimeOriginal Opt[string]; Latitude, Longitude Opt[float64]; Rating Opt[int]
	IsFavorite Opt[bool]; Visibility Opt[Visibility]; LivePhotoVideoID Opt[string] }
type AlbumInfoUpdate struct{ Name, Description Opt[string] }
type BulkIDResult struct{ ID string; Success bool; Error string }
type UploadCheckItem struct{ ID, Checksum string }
type UploadCheckResult struct{ ID string; Accept bool; Reason, AssetID string; IsTrashed bool }
type UploadRequest struct{ Filename string; FileCreatedAt, FileModifiedAt time.Time; DurationMS *int64; IsFavorite *bool
	Visibility Visibility; LivePhotoVideoID string; File *os.File }
type UploadResult struct{ ID string; Duplicate bool }
type Kind int // Unknown, Transient, Auth, Forbidden, NotFound(incl 400 "Not found or no … access"), BadRequest, Quota, TooLarge, Canceled
type APIError struct{ Method, Path string; Status int; Message string; Details []string; CorrelationID string }
func KindOf(err error) Kind
type Config struct{ BaseURL, APIKey, UserAgent string; InsecureTLS bool; JSONTimeout, StallTimeout time.Duration
	ResponseWait func(size int64) time.Duration; MaxRetries int; Logger *slog.Logger }
func New(cfg Config) (*Client, error) // *Client satisfies engine.Immich

// internal/engine
type Immich interface {
	Version(ctx) (immich.Version, error); APIKeyPermissions(ctx) ([]string, error); MyUserID(ctx) (string, error)
	GetAlbum(ctx, id) (immich.Album, error); UpdateAlbumInfo(ctx, id string, u immich.AlbumInfoUpdate) (immich.Album, error)
	AddToAlbum(ctx, albumID string, ids []string) ([]immich.BulkIDResult, error); RemoveFromAlbum(ctx, albumID string, ids []string) ([]immich.BulkIDResult, error)
	ListAlbumAssets(ctx, albumID string) ([]immich.Asset, error); ProbeAlbumAssets(ctx, albumID string, checksums []string) ([]immich.Asset, error)
	GetAsset(ctx, id string) (immich.Asset, error); UpdateAsset(ctx, id string, u immich.AssetUpdate) (immich.Asset, error)
	UploadCheck(ctx, items []immich.UploadCheckItem) ([]immich.UploadCheckResult, error)
	DownloadOriginal(ctx, id string, w io.Writer, onBytes func(int64)) (int64, error)
	Upload(ctx, r immich.UploadRequest, onBytes func(int64)) (immich.UploadResult, error)
	RestoreFromTrash(ctx, ids []string) error
}
type Side uint8 // A=0, B=1
type AlbumRef struct{ ID string }
// Engine.Pairs(ctx, static []Pair) ([]Pair, DiscoveryReport, error): static unless Options.Discovery != "".
type Pair struct{ Name string; A, B AlbumRef } // Name ^[a-z0-9][a-z0-9-]{0,62}$
type Options struct{ Concurrency int; TmpDir string; TmpBudgetBytes int64; DryRun, PropagateRemovals bool
	MaxRemovalRatio float64; MaxRemovalsMin int; ConflictWinner Side; SyncMetadata, SyncAlbumInfo, RestoreTrashed bool
	Discovery DiscoveryMode /* ""|all|marker */; DiscoveryMarker string; DiscoveryShare [2][]string; DiscoveryShareRole string /* editor|viewer */
	AllowMassRemoval bool; PendingMetaTimeout, CopyPhaseBudget time.Duration; CheckpointEvery int; CheckpointInterval time.Duration
	BackoffBase, BackoffMax, BlockedRecheck time.Duration; Now func() time.Time; OnProgress func(ProgressEvent) }
type Phase string // preflight|snapshot|plan|removals|metadata|albuminfo|copy|checkpoint|done
type ProgressEvent struct{ Pair string; Phase Phase; Done, Total int; Bytes int64 }
type Result string // ok|partial|failed|dry_run
type PerDir struct{ AtoB, BtoA int }
type SkipReason string // not_owner|blocked|backoff|target_hidden|target_trashed|server_hidden|offline|stale|quota|too_large|guard
type ItemError struct{ Key, Op string; Side Side; Kind immich.Kind; Msg string }
type RunSummary struct{ Pair string; Result Result; Started, Finished time.Time; AlbumIDs [2]string; AlbumSizes [2]int
	SnapshotConsistent, GuardTripped [2]bool; Bootstrap bool; Copied, Linked, Restored, Unlinked, MetaUpdates, AlbumInfoUpdated PerDir
	BytesDown, BytesUp int64; Pending, Deferred int; Skipped map[SkipReason]int; ItemErrors []ItemError /*cap 100*/; ItemErrorCount int; Mutations int }
var ErrConfig error
type PreflightError struct{ Side Side; Version immich.Version; Missing []string; Err error }
func RequiredPermissions(o Options) []string
func New(o Options, a, b Immich, st *state.Store, log *slog.Logger) (*Engine, error)
func (e *Engine) Preflight(ctx) error
func (e *Engine) RunPair(ctx, p Pair) (RunSummary, error)
func (e *Engine) PlanPair(ctx, p Pair) (planner.Plan, RunSummary, error)

// internal/engine/planner (pure; own Options subset mapped by engine)
type Key string
type Member struct{ Ref state.AssetRef; Owned, External bool; Meta state.Meta; LivePhotoVideoID string }
type Snapshot struct{ AlbumID string; Consistent bool; Groups map[Key][]Member } // [0]=primary
type ActionKind string // copy|probe_unlink|meta|link_repair|album_info|drop|block
type Action struct{ Kind ActionKind; Key Key; From, To uint8; Refs []state.AssetRef; Update *immich.AssetUpdate; TargetID string }
type Plan struct{ Removals, Meta, AlbumInfo, Copies []Action; GuardTripped [2]bool; Warnings []string }
func Build(a, b Snapshot, st *state.PairState, o Options, now time.Time) Plan
func MergeMeta(cur, base [2]*state.Meta, owned [2]bool, winner uint8, noBaseline [2]bool) (upd [2]*immich.AssetUpdate, newBase [2]*state.Meta, conflicts []string)

// internal/state (no engine import; [2] indexed A=0,B=1)
type PairState struct{ Schema int; Pair string; Fingerprint Fingerprint; AlbumIDs [2]string; Bootstrap bool; LastSuccess time.Time
	AlbumInfo [2]*AlbumInfo; Items map[string]*Item; Hashes map[string]HashEntry }
type Fingerprint struct{ UserIDs, AlbumIDs [2]string } // no URLs
type AlbumInfo struct{ Name, Description string }
type AssetRef struct{ ID, Checksum string }
type Item struct{ InBoth bool; Refs [2][]AssetRef; Meta [2]*Meta; MetaID [2]string; Intent *Intent; Pending *Pending; Blocked *Blocked; Failure *Failure }
type Meta struct{ Description string; TakenAt *time.Time; OffsetSec *int; Lat, Lon *float64; Rating *int; Visibility string }
type Intent struct{ To uint8; Started time.Time; Source AssetRef; SourceMeta Meta }
type Pending struct{ To uint8; TargetID string; Since time.Time }
type Blocked struct{ Op string; Side uint8; Reason string; Since time.Time }
type Failure struct{ Op string; Count int; Next time.Time; LastError string }
type HashEntry struct{ FileModifiedAt time.Time; Key string }
func Open(dir string) (*Store, error) // flock <dir>/.lock; ErrLocked
func OpenReadOnly(dir string) (*Store, error)
func (s *Store) Load(pair string) (*PairState, LoadStatus, error) // Fresh|Loaded|CorruptMovedAside|FingerprintReset
func (s *Store) Save(pair string, st *PairState) error // atomic, no ctx
func (s *Store) Reset(pair string) error
func (s *Store) PutMarker(pair, name string) error; func (s *Store) TakeMarker(pair, name string) (bool, error)
func (s *Store) Close() error

// internal/config + internal/daemon
type Instance struct{ URL, APIKey string; InsecureTLS bool }
type Config struct{ Instances [2]Instance; Pairs []engine.Pair; Interval time.Duration; Engine engine.Options; StateDir, HTTPAddr, LogFormat string; LogLevel slog.Level }
func Load(getenv func(string) (string, bool), readFile func(string) ([]byte, error)) (*Config, error)
type Runner interface{ Preflight(ctx) error; RunPair(ctx, engine.Pair) (engine.RunSummary, error) }
func daemon.New(cfg *config.Config, r Runner, reg prometheus.Registerer, log *slog.Logger) *Daemon
func (d *Daemon) Progress(ev engine.ProgressEvent); func (d *Daemon) Run(ctx) error; func (d *Daemon) Once(ctx) int; func (d *Daemon) Handler() http.Handler
```

---
## Appendix B — fakeimmich surface (internal/immich/fakeimmich)
```go
func New(tb testing.TB) *Server                 // httptest server; tb.Cleanup closes; URL() string
func (s *Server) URL() string
func (s *Server) AddUser(name string, perms ...string) (userID, apiKey string) // perms default ["all"]
func (s *Server) SetVersion(major, minor, patch int)
type SeedOpts struct{ Owner, Filename string; Content []byte; Type string /*IMAGE|VIDEO*/; Visibility immich.Visibility
	FileCreatedAt time.Time; LibraryID string /*external: checksum = sha1("path:"+filename)*/; Exif *immich.Exif; LivePhotoVideoID string }
func (s *Server) SeedAsset(o SeedOpts) (id string)
func (s *Server) CreateAlbum(owner, name string, assetIDs ...string) (id string)
func (s *Server) Share(albumID, userID, role string)            // role editor|viewer
func (s *Server) AddToAlbum(albumID string, ids ...string); func (s *Server) RemoveFromAlbum(albumID string, ids ...string)
func (s *Server) Trash(ids ...string); func (s *Server) Restore(ids ...string); func (s *Server) SetOffline(id string, off bool)
func (s *Server) SetVisibility(id string, v immich.Visibility)  // locked → removed from all albums (server rule)
func (s *Server) Edit(id string, f func(a *FakeAsset))           // direct mutation (favorite, exif, ...), bumps updatedAt
func (s *Server) Extract(id string)                              // simulate metadata extraction: fill exif dateTimeOriginal (fallback fileCreatedAt),
                                                                 // auto-link still+video sharing ContentID (hide motion + remove from all albums)
func (s *Server) SetContentID(id, cid string)
func (s *Server) Asset(id string) (FakeAsset, bool); func (s *Server) Album(id string) (FakeAlbum, bool)
func (s *Server) AlbumAssetIDs(albumID string) []string; func (s *Server) AssetsOf(owner string) []FakeAsset
type Fault struct{ Method, PathPrefix string; Status int; Times int; Delay time.Duration; TruncateAt int64; Body string }
func (s *Server) Inject(f Fault)                                 // next Times matching requests get fault (Status or truncated body)
func (s *Server) OnSearchPage(fn func(page int))                 // called before serving each search page (concurrent-mutation tests)
func (s *Server) Mutations() int                                 // count of mutating requests (POST/PUT/PATCH/DELETE except search/bulk-upload-check)
func (s *Server) Requests() []RecordedRequest                    // method, path, headers, body (multipart parsed: field order + file name/size)
```
Fidelity rules (must match real v3.4.1, verified by e2e):
1. Upload dedupe per (owner, checksum) for non-library assets INCLUDING trashed → 200 duplicate with existing id, request fields ignored.
2. bulk-upload-check matches caller's own assets (incl. trashed, incl. library) → reject/duplicate/assetId/isTrashed.
3. Album add bumps album updatedAt; remove does NOT. Add has no visibility check. Album add of id already present → error "duplicate".
4. Album remove (VERIFIED on v3.4.1 by e2e smoke): owner AND editor may remove anyone's asset (success). Viewer cannot add/remove → "no_permission".
5. Access failure → 400 {"message":"Not found or no <x> access"} (not 404). Unknown route → 404.
6. Search structured filter: albumIds.any, checksum.in, trashedAt.eq null, visibility.in; locked never returned to API keys;
   ordered fileCreatedAt asc, id asc; cursor = base64url({"offset":N}); size ≤1000; mixing flat fields with filter/orderBy/cursor → 400.
   withExif → exifInfo present (left join: assets without exif still returned with exifInfo null fields).
7. PUT /assets/{id}: owner only; latitude/longitude together; rating -1|1..5|null (0 → 400); dateTimeOriginal requires offset and sets
   timeZone to "UTC±H[:MM]" form; livePhotoVideoId link requires VIDEO owned by caller, hides motion (timeline→hidden), does NOT remove from albums.
8. Upload with livePhotoVideoId: same checks; motion hidden. Upload requires fileCreatedAt/fileModifiedAt with zone (naive → 400),
   rejects empty file (400) and unknown extension (400). isFavorite/visibility applied only on create.
9. isFavorite in responses false unless caller owns asset. Restore only restores trashed assets of caller.
10. Download original: owner, or album member (non-trashed asset), or still's livePhotoVideoId via album access.
11. /api-keys/me returns permissions; missing permission on an endpoint → 403 "Missing required permission: <perm>".
12. Album GET assetCount = non-trashed timeline+archive assets in album; albumUsers includes owner role first.
13. Teardown endpoints (smoke-verified): `DELETE /albums/{id}` owner only, assets kept; `DELETE /albums/{id}/user/{userId|me}`
    (owner → 400 "Cannot remove the last album owner", non-member → 400 "Album not shared with user"); `DELETE /assets
    {ids,force}` own assets only, force=false trashes; `GET /albums?assetId=` caller's albums holding the asset. Helper
    `LeaveAlbum(albumID, userID)`. Members may add assets they can read via a shared album to their own albums.

## Appendix C — rules for implementer agents
- Read this whole file first. Do not change semantics defined here; if something is impossible/wrong, implement the closest safe
  behavior, mark it `// DESIGN-DEVIATION:` in code, and list it in your final report.
- Only edit paths you own (given in your brief). Do not run `go mod tidy` or edit go.mod/go.sum unless you are the owner.
  Stdlib only beyond prometheus/client_golang and gopkg.in/yaml.v3; if you truly need a dep, report it instead.
- Code style: gofmt, `go vet` clean, small files by concern, doc comments on exported identifiers, no global mutable state,
  context-first APIs, wrap errors with `%w`, structured logs via `*slog.Logger` with keys pair, side, key, asset_id, op, kind, phase.
- Tests: table-driven, `t.Parallel()` where safe, no sleeps for sync (use channels/fake clocks via Options.Now), `-race` clean.
- Final report: files created, test command + result, deviations, open issues. Keep it short.

## Appendix D — precise planner / engine semantics (W2a implements planner+state, W2c engine)
### Planner type extensions (W2a may ADD fields; must keep existing ones)
- `Member` + `Extracted bool` (target exif dateTimeOriginal != nil), `Visibility immich.Visibility`, `FileCreatedAt time.Time`.
- `Snapshot` + `AlbumName, AlbumDescription string`.
- `Action` + `Updates [2]*immich.AssetUpdate`, `TargetIDs [2]string`, `NewBase [2]*state.Meta`, `NewBaseIDs [2]string`,
  `ClearPending bool`, `AlbumUpdates [2]*immich.AlbumInfoUpdate`, `NewAlbumBase [2]*state.AlbumInfo`, `Reason string`.
  (single `Update/TargetID` stay for copy/link_repair convenience.)
- `Plan` + `Paired []Key` (keys now in both → engine sets InBoth, refreshes Refs/MetaID), `Drops []Key`, `Skipped map[string]int`
  (reason → count; reasons = engine.SkipReason strings), `Pending int`, `RemovalCandidates [2]int`.
### Build rules
Let inA/inB = key present in snapshot groups; inS = `st.Items[key].InBoth`.
1. (1,1,*): Paired. If `SyncMetadata`: emit one `meta` action per key when any side needs a write OR baselines change
   (Updates per side + NewBase per side). `noBaseline[s] = Meta[s]==nil || MetaID[s] != primary(s).Ref.ID`.
   If Item.Pending (To=t): if !target.Extracted && now-Pending.Since < PendingMetaTimeout → skip key (`Pending++`), no action.
   Else EXIF fields (description,date+offset,lat/lon,rating) source-wins onto t where they differ; visibility normal merge;
   `ClearPending=true` when no EXIF diff remains. Link repair: source primary has LivePhotoVideoID, target primary has none,
   target owned → `link_repair` action (From=source,To=target, TargetID=target still id).
2. (1,0,0)/(0,1,0): `copy` From→To, Refs = source group refs, TargetID="" . Skip if Item.Failure.Next > now (`backoff`) or Blocked.
3. (1,0,1)/(0,1,1): if !PropagateRemovals → nothing (S untouched). Else candidate removal: `probe_unlink` with
   From = side where key vanished (to probe), To = side to unlink from, Refs = To-side group refs.
   Guard per To side: candidates[To] > max(MaxRemovalsMin, ceil(MaxRemovalRatio*|S|)) && !AllowMassRemoval →
   drop ALL removal actions of the pair, GuardTripped[To]=true, warning, `guard` skip count.
4. (0,0,1): `Drops` (engine deletes item incl. failures/pending/baselines).
5. Blocked items: skip (count `blocked`) unless now-Blocked.Since > BlockedRecheck or row is (1,1)/(0,0) → treat normally (engine clears Blocked).
6. Album info (if SyncAlbumInfo): per-side baselines `st.AlbumInfo[s]`; same per-field rules as metadata (no baseline → fill-empty
   description only; never rename without baseline). Emit `album_info` action with AlbumUpdates/NewAlbumBase when needed.
7. Deterministic output: sort actions by Key. Planner never mutates `st`.
### MergeMeta rules (per field f ∈ description, date(TakenAt+OffsetSec), location(Lat,Lon pair), rating, visibility)
- known[s] = base[s]!=nil && !noBaseline[s]. eq = normalized equality (description trim? NO trim; null==""; date instant equal AND
  offset equal (nil offset == 0); lat/lon |Δ|<1e-6 both; rating nil==nil; visibility string).
- both known: ch[s] = !eq(cur[s].f, base[s].f). none → no write. one (s) → write cur[s].f to other side o (if owned[o], else
  conflict note "not_owner" and no write). both & !eq(cur) → winner's value to loser (if owned). both & eq(cur) → none.
- not both known: eq(cur) → none; description/rating/location: fill the side whose value is empty (""/nil) from the other (if owned);
  date/visibility: no write, conflict note "diverged:<field>".
- Clearing: if the value to write is empty for location → no write, note "unsupported:clear_location".
- newBase[s] = cur[s] with written fields replaced by written value (engine overwrites with PUT read-back).
- Updates only contain changed fields; date written as RFC3339 with numeric offset from OffsetSec (nil→Z).
- Visibility only timeline|archive are writable; anything else → no write.
### Engine rules (W2c)
- Album resolution per side: configured AlbumRef.ID (empty → ErrConfig; state.AlbumIDs is never a fallback).
  Role must be owner|editor else ErrConfig. Album NotFound for an id → ErrConfig ("album <id> gone on side X; refusing to recreate").
- Fingerprint{UserIDs, AlbumIDs} compared after resolution; mismatch with a non-zero stored fingerprint → move state aside, fresh
  (LoadStatus FingerprintReset, Bootstrap=true). W2a provides `func (s *Store) Reconcile(pair string, st *PairState, fp Fingerprint) (*PairState, LoadStatus, error)`.
- Markers read at start of RunPair: "reset" → Store.Reset then fresh state, marker removed only after the reset succeeded;
  "approve-removals" → AllowMassRemoval for this run only, marker removed right before execution (a failed load/snapshot/plan keeps
  it). A non-regular file at a marker path is an error.
- Bootstrap cleared (false) after the first cycle with Result ok and zero item errors.
- Snapshot → Members: Owned = asset.OwnerID == myUserID; External = LibraryID != nil; Meta normalized from exif + visibility; Extracted = Exif != nil && Exif.DateTimeOriginal != nil. Group primary = owned first, then lowest ID.
- Probe classification as DESIGN step 5. Unlink results: success / not_found → ok; no_permission → Blocked{Op:"unlink",Reason:"no_permission"}.
- After successful unlink → delete the Item entirely (key is now absent from both albums).
- After copy success + album add → Item.InBoth=true, Refs both sides, Pending (if created), baseline for visibility, Intent cleared.
- Failure backoff: Failure.Count++, Next = now + min(BackoffMax, BackoffBase*2^(Count-1)); KindCanceled never recorded.

## Appendix E — as-built notes: planner + state (from W2a)
- state: `NewPairState`, `(*PairState).Item(key)` get-or-create, `Delete(key)`, `InBothCount()`; `(*Store).Reconcile(pair, st, fp)`; `ErrReadOnly`.
  Files `<dir>/pairs/<pair>.json`; markers `<pair>.<name>.marker` (names match pair regex); aside suffixes `.corrupt-|.fpreset-|.reset-<unixnano>`.
  Reset moves aside (never deletes). Read-only Load of a corrupt file returns fresh + CorruptMovedAside without moving.
- planner additions: `Options.PendingMetaTimeout`; `Member.Extracted/Visibility/FileCreatedAt`; `Snapshot.AlbumName/AlbumDescription`;
  `Action.Updates/TargetIDs/NewBase/NewBaseIDs/ClearPending/AlbumUpdates/NewAlbumBase/Reason`; `Plan.Paired/Drops/Skipped(non-nil)/Pending/RemovalCandidates`;
  helpers `OffsetSeconds, FormatDate, EqualMeta, EqualDescription, EqualDate, EqualLocation, EqualRating, EqualVisibility`.
- Action shapes: `link_repair` lives in `Plan.Meta` (From/To, Refs=[source primary], TargetID=target still). `meta`: From/To unset; use
  Updates[side]/TargetIDs[side]/NewBase[side] bound to NewBaseIDs[side]; emitted on any write, baseline change/rebind, or ClearPending.
  album_info: Key "", AlbumUpdates[side]. Drops only in Plan.Drops. probe_unlink: From = side to probe, To = side to unlink, Refs = To-side refs.
- Semantics chosen: PropagateRemovals=false suppresses removals only; BlockedRecheck<=0 = never expires; backoff also applies to removals
  (Failure.Op unlink|probe), guard counts candidates before backoff filter; pending source-wins = EXIF fields only (writes empty source
  values too, except location clear); ClearPending when no EXIF write this cycle; visibility written only when both sides timeline|archive;
  clearing a date → note `unsupported:clear_date`, no write; Options.Bootstrap unused by planner (noBaseline derives from state).

## Appendix F — real-server findings (W1b e2e smoke, v3.4.1) — all other "Verified API facts" CONFIRMED
- Editors can remove any member asset from an album (fakeimmich rule 4 corrected). `no_permission` on unlink only for viewers/no access.
- Legacy flat withExif did not drop not-yet-extracted assets in practice; structured search still used.
- Upload naive datetime → 400; rating 0 → 400; PUT dateTimeOriginal +02:00 → timeZone "UTC+2".
- Fixtures live in test/e2e/testutil/testdata/ (embed). `testutil.E2EEnv()` reads `.e2e/env`. Cold start ≈12s with images cached.
- Spec check: `UpdateAssetDto.description` is NON-nullable → clear asset description by sending `""` (planner fixed). `rating` and
  `livePhotoVideoId` are nullable. `UpdateAlbumDto.description` is nullable.

## Appendix G — as-built notes: immich client + fakeimmich (from W1a)
- Extras: `Config.BackoffBase/BackoffMax`, `Client.BaseURL()`, `NormalizeBaseURL()`; fake `RecordedRequest.Query/ContentLength/BodyBytes`,
  `FakeAlbum.CreatedAt`, `Fault.Header`. Upload streams head+file+trailer reader with exact Content-Length; onBytes counts file bytes read
  (retries count again). Retry-After fractional seconds, capped 5m. Client retries 429/500/502/503/504 only.
- Fake: `OnSearchPage(page)` 0-based, called WITHOUT server lock (may call mutators); `Edit(id,f)` holds lock (f must not call Server
  methods); Fault.PathPrefix matches with/without `/api`; TruncateAt only affects downloads; Times<=0 → 1; faulted requests not counted
  in Mutations(). Locked assets inaccessible (400). Non-owner PUT asset → denied. Restore of non-owned id → 400. Album PATCH owner|editor.
  Date PUT updates exif dateTimeOriginal+timeZone ("UTC+2") but not fileCreatedAt. Flat-only search bodies → 400 (not implemented).
