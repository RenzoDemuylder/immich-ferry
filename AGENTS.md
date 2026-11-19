# AGENTS.md

Guidance for AI coding agents working in this repository.

## What this is

`immich-ferry` is a Go daemon that keeps one album (or several album pairs) in sync **bidirectionally** between two
Immich instances using only the REST API. It runs unattended as a Kubernetes pod (Helm chart) or a plain Docker
container. Target: Immich **v3.4.1**, minimum v3.2 (structured search), major version must be 3.

**`docs/DESIGN.md` is the source of truth**: verified API facts, the sync algorithm, the "what can go wrong" table,
pinned Go contracts, and real-server findings (appendices). Read the relevant sections before changing behaviour,
and update DESIGN.md in the same change when you alter semantics.

## Layout

| Path | Purpose |
|---|---|
| `internal/engine/discover.go` | Album discovery (`SYNC_DISCOVERY` all/marker): pairing by name, counterpart creation, tombstones |
| `cmd/immich-ferry/` | CLI: `run` (daemon), `once`, `validate`, `healthcheck`, `version`, `approve-removals`, `state show\|reset` |
| `internal/immich/` | HTTP client: DTOs, retries/backoff, error kinds, stall watchdog, streamed multipart upload |
| `internal/immich/fakeimmich/` | In-memory fake Immich used by unit tests (fidelity rules in DESIGN Appendix B) |
| `internal/engine/` | Orchestration: preflight, snapshot, removal probe, copy/transfer, live photos, metadata, album info |
| `internal/engine/planner/` | **Pure** logic: truth table, guards, per-side metadata merge, normalization. No IO. |
| `internal/state/` | Per-pair JSON state, atomic saves, flock on `<dir>/.lock`, markers |
| `internal/config/` | Env + YAML config, `*_FILE` secrets, validation |
| `internal/daemon/` | Loop, signals, `/healthz` `/readyz` `/metrics` `/status`, Prometheus metrics |
| `test/e2e/` | Two-instance compose stack, testutil, e2e scenarios (`e2e` tag), kind/Helm test (`e2e_kind` tag) |
| `../helm-charts/charts/immich-ferry/` | Helm chart, **not in this repo**: lives in `github.com/RenzoDemuylder/helm-charts` (see below) |
| `examples/docker/` | Plain docker run / compose examples |

## Commands

```sh
make build          # bin/immich-ferry
make test           # go test -race -count=1 ./...
make lint           # gofmt check + go vet
make image          # docker image immich-ferry:{dev,latest,<version>,<appVersion>}
make chart-lint     # helm lint --strict of ../helm-charts/charts/immich-ferry with each ci values file (HELM_CHARTS=...)
make e2e            # compose up two Immich instances, go test -tags e2e, tear down (E2E_KEEP=1 keeps the stack)
make e2e-kind       # optional: dedicated kind cluster immich-ferry-e2e + Helm install (KEEP_CLUSTER=1 keeps it)
make demo           # bring up the stack, create a demo album on A (empty one on B), sync once, print URLs + credentials
```

Before finishing any change: `make lint test`. If you touched the client, engine, CLI, Dockerfile or chart, also run
`make e2e` (needs Docker; ~4 min with images cached). Run `make chart-lint` for chart changes. Also vet the tagged
tests: `go vet -tags e2e ./test/e2e/... && go vet -tags e2e_kind ./test/e2e/...`.

E2E details: compose project `immich-ferry-e2e`, host ports `127.0.0.1:22283` (A), `22284` (B), `22880` (daemon),
override with `IMMICH_A_PORT`/`IMMICH_B_PORT`/`SYNC_PORT`; Immich version via `IMMICH_VERSION`. `make e2e-up` writes
admin credentials and keys to `.e2e/env` (gitignored); `testutil.E2EEnv()` reads it.

## Safety invariants (do not break)

These protect users' photo libraries. Each has tests; keep them green and add a regression test for any bug you fix.

- **Never delete or trash assets.** Removal propagation only unlinks assets from the other album. Sole exception: the discovery
  teardown with `SYNC_DISCOVERY_TEARDOWN_TRASH` (off by default) trashes (`force:false`, never permanent) assets of the
  created album that the sync user owns, that `Item.Ours` proves this tool uploaded, and that are in no other album it can see.
- **Removals are probe-confirmed and fail safe.** A candidate removal is re-checked on the side where it vanished;
  hidden/offline/still-visible/unknown (any check error) → skip or block, never unlink.
- **Mass-removal guard**: more than `max(SYNC_MAX_REMOVALS_MIN, ratio × tracked)` removals on a side → no removals
  until `approve-removals`. An empty or lost state never removes anything (union semantics).
- **Metadata uses per-side baselines bound to asset ids.** With no baseline, only fill empty fields (description,
  rating, location); never overwrite dates/favorite/visibility. Baselines after a write = snapshot values with only the
  written fields taken from the read-back; on partial failure keep the old baseline for unwritten changes.
- **Source-wins (pending) metadata only for assets this tool uploaded** (`Intent.UploadStarted` marker or an upload
  that returned `created`/our own retried duplicate). Reused pre-existing target assets go through the normal merge.
- **Dry-run / `validate` / `PlanPair` never mutate** either server, never write or move state files.
- **State**: atomic save (tmp + fsync + rename + dir fsync), `Save` takes no context, one process per state dir
  (flock). Context cancellation is never recorded as an item failure.
- **Transfers**: stream to a temp file, verify SHA1 against the key before uploading, always remove temp files,
  respect `TmpBudgetBytes` (unknown size reserves the full budget; larger than budget → `too_large`, no download).
- **Live photos**: upload the motion part hidden first, never add motion parts to albums, never link a visible
  (timeline/archive) target motion.
- **Configured albums are never created**: a vanished configured album is a pair error.
- **Album discovery** (`SYNC_DISCOVERY`): pairs only by exact name, skips anything ambiguous, never retries album creation,
  confirms a missing pinned album by id before unpairing, and never re-creates a deleted counterpart (tombstone). Unpairing
  leaves albums and assets untouched, except the **teardown**: when everyone a created, shared album (`DiscoveredPair.Created`)
  was shared with has left it (confirmed by GET), the sync user leaves the original album, optionally trashes its own uploads,
  deletes the created album and unpairs; steps are persisted and resumed, and the pair is never synced again. With
  `SYNC_DISCOVERY_TEARDOWN=false` no teardown starts (the pair keeps syncing) and one in progress is paused, never synced. With
  `SYNC_DISCOVERY_DEDICATED_USER`, an unpinned album the sync user owns without other members is never synced or paired. Dry-run/`validate` never create albums or write `discovery.json`. With
  `SYNC_DISCOVERY_SHARE_*`, a created album is shared in the create request itself (never created unshared); members of
  existing albums are never changed (the sync user only ever removes itself, in a teardown).
- **Favorites are never synced** (per user in Immich): no `isFavorite` on uploads or asset updates.

## Immich API gotchas (details and evidence in DESIGN.md)

- Album responses contain **no assets** (v3). List album assets only via the structured `POST /search/metadata`
  body (`filter`/`orderBy`/`cursor`); never mix with deprecated flat fields (400). Paging is offset-based.
- Upload: `fileCreatedAt`/`fileModifiedAt` must carry a zone; no `deviceId`/`deviceAssetId`; do not send
  `x-immich-checksum`. A "duplicate" can be a **trashed** asset; request fields are not applied on duplicates.
- Access errors are `400 "Not found or no … access"` → `KindNotFound`. Editors can remove any member asset.
- `UpdateAssetDto.description` is non-nullable: clear it with `""`. Rating 0 is invalid. Writing a date turns an
  IANA zone into `UTC±N` (or a GPS-derived zone), so compare dates by instant + UTC offset.
- Redirects are not followed (API key safety).

When upgrading the supported Immich version: bump `IMMICH_VERSION`, run `make e2e` (the smoke test
`TestSmokeAPIFacts` re-verifies every API fact), and update DESIGN.md "Verified API facts" plus fakeimmich if behaviour changed.

## Conventions

- Go 1.27, module `immich-ferry`. Dependencies: stdlib plus `prometheus/client_golang` and `gopkg.in/yaml.v3` only.
  Ask before adding a dependency.
- gofmt, `go vet` clean, context-first APIs, wrap errors with `%w`, classify client errors with `immich.KindOf`.
- Logging via `*slog.Logger` with keys `pair, side, key, asset_id, op, kind, phase`. Never log API keys or URL userinfo.
- Tests: table-driven, `-race` clean, no sleeps for synchronisation (use channels and `Options.Now` fake clocks).
  Engine behaviour is tested against `fakeimmich`; if you change the fake, the behaviour must match the real server
  (prove it in `test/e2e/smoke_test.go`).
- Keep the planner pure: no IO, never mutate the passed state.

## Change checklists

- **New/changed setting**: `internal/config` (env + YAML + validation + defaults) → README configuration table →
  `examples/docker/*` → the chart in helm-charts (`values.yaml`, `templates/deployment.yaml`, tests; see below).
- **New/changed metric**: `internal/daemon/metrics.go` → README metrics list → DESIGN metrics list.
- **New required API permission**: `engine.RequiredPermissions` → README permissions table → e2e keys use
  `RequiredPermissions` automatically.
- **Chart change**: see "Helm chart" below; optionally `make e2e-kind` here.
- **New release**: push the image `ghcr.io/renzodemuylder/immich-ferry:<version>`, then bump `image.tag`, `appVersion`
  and `annotations.images` in the chart (helm-charts repo).

## Helm chart

The chart is **not** in this repository. It lives in `github.com/RenzoDemuylder/helm-charts` as `charts/immich-ferry`
(published to `oci://ghcr.io/renzodemuylder/charts/immich-ferry`), checked out next to this repo at `../helm-charts`
(`HELM_CHARTS=` overrides the path for `make chart-lint`; `IMMICH_FERRY_CHART=` for the kind e2e test).

When a Claude Code session in this repository has to change the chart (new setting, new env var, probes, image tag...):

- make the change in `../helm-charts/charts/immich-ferry`, on a branch of the helm-charts repo, never by adding a chart here;
- follow that repo's `create-helm-chart` skill (`../helm-charts/.claude/skills/create-helm-chart/SKILL.md`) and its
  `CLAUDE.md`: values layout and `@param` annotations, `common` helpers, secure defaults, `tests/`, `ci/` values,
  regenerated README, `version` bump in `Chart.yaml`;
- validate there with `make test` (and `make e2e E2E_CHARTS=immich-ferry` when templates change; the image must be
  pullable or `kind load`ed into the `chart-testing` cluster);
- commit the app change here and the chart change in helm-charts separately.

## Environment etiquette

Only touch the compose project `immich-ferry-e2e` and the kind cluster `immich-ferry-e2e`. Never modify, stop, or
delete other Docker containers, compose projects, or kind clusters on the machine. Always pass an explicit
`--kube-context`/`--context` to kubectl and helm. Put scratch files outside the repo.
