# Sync flow — every possibility in one sync period

Flow diagrams of what `immich-ferry run` does each `SYNC_INTERVAL`. They illustrate
[DESIGN.md](DESIGN.md) ("Sync algorithm", Appendix D); when the two disagree, DESIGN.md and the code win.
Code anchors: `internal/daemon/daemon.go` (loop), `internal/engine/discover.go`, `internal/engine/run.go`
(cycle), `internal/engine/planner/` (truth table), `probe.go`, `copy.go`, `livephoto.go`, `metadata.go`, `albuminfo.go`.

Diagrams:

1. [Daemon cycle](#1-daemon-cycle)
2. [Album discovery](#2-album-discovery-sync_discovery--allmarker)
3. [Pair run](#3-pair-run-runpair)
4. [Per-key decision (planner)](#4-per-key-decision-planner)
5. [Removal probe](#5-removal-probe-per-candidate)
6. [Copy one asset](#6-copy-one-asset)
7. [Metadata merge per field](#7-metadata-merge-per-field)
8. [End of pair run](#8-end-of-pair-run-state-and-result)

## 1. Daemon cycle

```mermaid
flowchart TD
    start(["Cycle start (timer fired)"]) --> pf["Preflight both instances<br/>version 3.2 ≤ v #lt; 4, API key permissions, user id"]
    pf -->|"error"| pferr["Log error, preflight_ok metric = 0 for failing side"]
    pferr --> shortwait["wait = min(SYNC_INTERVAL, 1m)"]
    pf -->|"ok"| disc{"SYNC_DISCOVERY?"}
    disc -->|"off"| cfg["Pairs = configured pairs<br/>(SYNC_A_ALBUM_ID / SYNC_B_ALBUM_ID or YAML)"]
    disc -->|"all / marker"| dd["Album discovery (diagram 2)"]
    dd -->|"GET /albums failed / state error"| derr["Log, discovery error metric<br/>no pair runs this cycle"]
    derr --> shortwait
    dd -->|"ok"| dpairs["Pairs = discovered pairs<br/>forget metrics/status of unpaired pairs"]
    cfg --> loop
    dpairs --> loop
    loop{"Next pair?"} -->|"yes"| ctxchk{"Shutdown signal?"}
    ctxchk -->|"no"| run["RunPair (diagram 3)"]
    run -->|"ok / partial"| okp["Record summary + metrics"]
    run -->|"error"| errp["Record pair error (other pairs still run)"]
    okp --> loop
    errp --> loop
    ctxchk -->|"yes"| stop
    loop -->|"no more"| normwait["wait = SYNC_INTERVAL ± 10% jitter"]
    normwait --> sleep["Sleep (interruptible)"]
    shortwait --> sleep
    sleep -->|"timer"| start
    sleep -->|"SIGTERM / SIGINT"| stop(["Shutdown: final state already saved,<br/>HTTP server stopped"])
```

## 2. Album discovery (`SYNC_DISCOVERY` = all|marker)

Runs once per cycle after preflight. Selected = role owner/editor AND (mode `all` OR description contains
`SYNC_DISCOVERY_MARKER` on either side).

```mermaid
flowchart TD
    s(["Discovery"]) --> list["GET /albums on A and B<br/>load discovery.json (user-id mismatch / corrupt → moved aside, fresh)"]
    list -->|"error"| fail(["Discovery fails → no pair this cycle"])
    list --> pinned["For each pinned pair"]

    pinned --> inlist{"Both albums in listing?"}
    inlist -->|"yes"| ed{"Owner/editor on both sides?"}
    ed -->|"no"| unpairT
    ed -->|"yes"| sel{"Selected on either side?"}
    inlist -->|"no"| confirm["GET /albums/{id} for missing one"]
    confirm -->|"lookup error"| keepskip["Keep pair, skip it this cycle"]
    confirm -->|"found"| ed
    confirm -->|"not found"| unpairT["Unpair + tombstone the surviving album<br/>(pair state moved aside; albums/assets untouched)"]
    sel -->|"yes"| keep["Keep pair → runs this cycle"]
    sel -->|"no"| unpair["Unpair, no tombstone"]

    keep --> unp
    keepskip --> unp
    unpairT --> unp
    unpair --> unp
    unp["For each unpinned selected album x on side s"] --> named{"Named and name unique among<br/>unpinned editable albums on s?"}
    named -->|"no"| skip1["Skip (ambiguous / unnamed)"]
    named -->|"yes"| other{"Same-name albums on other side o"}
    other -->|"exactly 1 free, none blocked"| pair["Pair them; clear tombstones of both"]
    other -->|">1 free or any pinned/viewer"| skip2["Skip (ambiguous)"]
    other -->|"none"| tomb{"x tombstoned?"}
    tomb -->|"yes"| skip3["Skip: counterpart deleted, never re-created"]
    tomb -->|"no"| dry{"Dry run?"}
    dry -->|"yes"| would["Report wouldCreate"]
    dry -->|"no"| share{"SYNC_DISCOVERY_SHARE_o set?"}
    share -->|"yes"| resolve["Resolve emails via GET /users"]
    resolve -->|"error"| noc["Not created, error noted, retry next cycle"]
    resolve -->|"ok"| create
    share -->|"no"| create["POST /albums on o<br/>(name + description of x, albumUsers in same request)"]
    create -->|"ok"| pair2["Pair"]
    create -->|"error (never retried)"| adopt["Error noted; if it was created anyway,<br/>adopted by name next cycle"]

    pair --> gc
    pair2 --> gc
    skip1 --> gc
    skip2 --> gc
    skip3 --> gc
    would --> gc
    noc --> gc
    adopt --> gc
    gc["Drop tombstones whose album is gone<br/>save discovery.json (not in dry run)"] --> out(["Pairs + DiscoveryReport → /status, metrics"])
```

## 3. Pair run (`RunPair`)

Phase order: preflight → markers → state → albums → snapshot → plan → removals → metadata → album info → copies → save.

```mermaid
flowchart TD
    s(["RunPair(pair)"]) --> mk{"Mutating run?<br/>(not dry run / validate / PlanPair)"}
    mk -->|"yes"| reset{"reset marker?"}
    reset -->|"yes"| doreset["Discard pair state (fresh, bootstrap)"]
    reset -->|"no"| appr
    doreset --> appr{"approve-removals marker?"}
    appr -->|"yes"| allow["AllowMassRemoval for this run only (consumed)"]
    appr -->|"no"| load
    allow --> load
    mk -->|"no"| peek["Peek state (never moved / written)"]
    peek --> albums
    load["Load state<br/>corrupt → moved aside, fresh"] --> albums

    albums["Resolve album per side by id"] --> aok{"Album OK?"}
    aok -->|"no id configured"| cerr(["Config error"])
    aok -->|"not found"| gone(["Pair error: album gone, never recreated"])
    aok -->|"viewer role"| cerr
    aok -->|"same album claimed by 2 pairs"| cerr
    aok -->|"ok"| fp{"Fingerprint (user ids + album ids)<br/>matches state?"}
    fp -->|"no"| fresh["State moved aside → fresh, bootstrap"]
    fp -->|"yes"| snap
    fresh --> snap

    snap["Snapshot A and B fully<br/>key = checksum (external library: SHA1 of content, cached)"] -->|"error"| serr(["Pair error"])
    snap --> recover["Recover leftover upload Intents"]
    recover --> plan["Planner.Build (diagram 4)"]
    plan --> inc{"Snapshot incomplete on a side?<br/>(external asset hash failed)"}
    inc -->|"yes"| filt["Drop copies into / removals probing that side,<br/>drop all Drops → skip snapshot_incomplete"]
    inc -->|"no"| dr
    filt --> dr{"Mutating?"}
    dr -->|"no"| report(["Return plan counts: nothing written"])
    dr -->|"yes"| rem["Removals phase (diagram 5)"]
    rem --> cp1["Checkpoint"] --> c1{"Canceled?"}
    c1 -->|"no"| meta["Metadata phase (diagram 7)<br/>+ live-photo link repair"]
    meta --> cp2["Checkpoint"] --> c2{"Canceled?"}
    c2 -->|"no"| ainfo["Album info phase (name, description)"]
    ainfo --> cp3["Checkpoint"] --> c3{"Canceled?"}
    c3 -->|"no"| copy["Copy phase (diagram 6)<br/>batches of 100, Concurrency workers,<br/>until SYNC_COPY_BUDGET (20m) expires"]
    c1 -->|"yes"| defer["All copies deferred"]
    c2 -->|"yes"| defer
    c3 -->|"yes"| defer
    copy --> fin["End of run (diagram 8)"]
    defer --> fin
```

## 4. Per-key decision (planner)

`inA`/`inB` = key in the album snapshot of that side; `inS` = key was in both albums after the last success
(`Item.InBoth`). Pure function: no IO, never mutates state.

```mermaid
flowchart TD
    k(["Key"]) --> blk{"Blocked?"}
    blk -->|"yes, row unchanged and #lt; 24h (BlockedRecheck)"| sb["Skip: blocked"]
    blk -->|"no / expired / row now (1,1) or (0,0)"| row{"inA, inB, inS"}

    row -->|"1,1,*"| paired["Paired: S += key, refresh refs"]
    paired --> sm{"SYNC_METADATA?"}
    sm -->|"no"| none1["Nothing"]
    sm -->|"yes"| pend{"Pending (we uploaded it)?"}
    pend -->|"yes, target EXIF not extracted, #lt; SYNC_PENDING_META_TIMEOUT"| sp["Skip this cycle (pending)"]
    pend -->|"yes, extracted or timed out"| sw["Source-wins for EXIF fields<br/>visibility: normal merge<br/>clear Pending when no EXIF diff left"]
    pend -->|"no"| merge["Normal per-field merge (diagram 7)"]
    sw --> lr
    merge --> lr{"Source still has motion,<br/>target still has none, target owned?"}
    lr -->|"yes"| repair["+ link_repair action"]
    lr -->|"no"| metaact["meta action if any write or baseline change"]
    repair --> metaact

    row -->|"1,0,0"| cab["Copy A → B"]
    row -->|"0,1,0"| cba["Copy B → A"]
    cab --> bo{"Failure backoff active?"}
    cba --> bo
    bo -->|"yes"| sbo["Skip: backoff"]
    bo -->|"no"| copyact["copy action"]

    row -->|"1,0,1"| rB["Vanished from B → candidate: probe B, unlink from A"]
    row -->|"0,1,1"| rA["Vanished from A → candidate: probe A, unlink from B"]
    rB --> prop{"SYNC_PROPAGATE_REMOVALS?"}
    rA --> prop
    prop -->|"no"| ign["Nothing; S untouched (never resurrected)"]
    prop -->|"yes"| guard{"Candidates on that side #gt;<br/>max(SYNC_MAX_REMOVALS_MIN, ratio × |S|)<br/>and not approved?"}
    guard -->|"yes"| gt["Guard tripped: drop ALL removals of the pair,<br/>pair reports error until approve-removals"]
    guard -->|"no"| probeact["probe_unlink action"]

    row -->|"0,0,1"| drop["Drop item (GC failures, pending, baselines)"]
    row -->|"empty S (first run / lost / reset)"| union["Never a removal: everything is a copy (union)"]
```

## 5. Removal probe (per candidate)

Probe runs on the side where the key vanished. Any doubt → no unlink. Assets are never deleted or trashed;
an unlink only removes the asset from the other album.

```mermaid
flowchart TD
    c(["Candidate removal"]) --> refs{"Recorded refs on probe side?"}
    refs -->|"no"| skipnr["Skip (warn)"]
    refs -->|"yes"| q["Probe album members by checksum"]
    q -->|"error"| perr["Record failure + backoff, no unlink"]
    q --> cls{"Classify probed members"}
    cls -->|"any still visible (timeline/archive)"| stale["Skip: stale (snapshot glitch)"]
    cls -->|"any offline"| off["Skip: offline (this cycle)"]
    cls -->|"hidden/locked member"| hid["Block: server_hidden (auto-linked motion)"]
    cls -->|"not a member, or member trashed"| empty{"No member at all?"}
    empty -->|"trashed member"| ul
    empty -->|"none"| he["Check: asset exists hidden outside the album?"]
    he -->|"error"| perr
    he -->|"yes"| hid
    he -->|"no"| ul["Unlink from the other album"]
    ul -->|"ok / not found"| delitem["Delete item: key gone from both"]
    ul -->|"no_permission"| bperm["Block: no_permission"]
    ul -->|"other error"| uerr["Record failure + backoff"]
```

## 6. Copy one asset

```mermaid
flowchart TD
    c(["copy s → t"]) --> stop{"Budget expired / canceled /<br/>5 consecutive transient errors?"}
    stop -->|"yes"| deferred["Deferred → next cycle"]
    stop -->|"no"| quota{"Quota hit on t this run?"}
    quota -->|"yes"| sq["Skip: quota"]
    quota -->|"no"| chk["bulk-upload-check on t (by checksum)"]
    chk -->|"error"| fail
    chk -->|"exists on t"| get["GET existing asset on t"]
    chk -->|"accept"| sz{"Size vs SYNC_TMP_BUDGET"}

    get -->|"not found (locked)"| bh["Block: target_hidden"]
    get -->|"visibility not timeline/archive"| bh
    get -->|"trashed"| rt{"SYNC_RESTORE_TRASHED and not bootstrap?"}
    rt -->|"no"| bt["Block: target_trashed"]
    rt -->|"yes"| restore["Restore + re-GET"]
    restore -->|"still trashed"| bt
    restore -->|"error"| fail
    restore -->|"ok"| lpx
    get -->|"visible"| lpx{"Live photo: source has motion,<br/>target still unlinked and owned?"}
    lpx -->|"yes"| link["Link motion (diagram 6b)"]
    lpx -->|"no"| add
    link -->|"ok / motion skipped (warn)"| add
    link -->|"error"| fail

    sz -->|"larger than budget"| tl["Fail: too_large, 30-day backoff"]
    sz -->|"fits"| lp{"Live photo still?"}
    lp -->|"yes"| motion["Ensure motion on t first (hidden) (6b)"]
    lp -->|"no"| acq
    motion -->|"ok / skipped (warn: still alone)"| acq
    motion -->|"error"| fail
    acq["Acquire tmp byte budget"] --> dl["Stream download to temp file + SHA1"]
    dl -->|"SHA1 ≠ key (retry 3) / error"| fail
    dl -->|"ok"| intent["Write-ahead Intent (UploadStarted), save state"]
    intent --> up["Upload from file (visibility, livePhotoVideoId)<br/>temp file always removed"]
    up -->|"created"| addn["Pending = source-wins metadata"]
    up -->|"duplicate (our retried upload)"| get
    up -->|"413"| tl
    up -->|"quota"| qset["Stop uploads to t this run, skip: quota"]
    up -->|"other error"| fail
    addn --> add["Queue album add (flush every 100)"]
    add -->|"ok"| done["Item InBoth, refs both sides,<br/>visibility baseline, Intent cleared"]
    add -->|"error"| fail
    fail["Record failure: backoff 15m → 24h exp<br/>(cancellation never recorded)"]
```

### 6b. Live-photo motion

Motion skips (warn, still copied/linked alone) are not failures; other errors fail the copy.

```mermaid
flowchart TD
    m(["Motion part of source still"]) --> src{"GET source motion"}
    src -->|"not found"| skip["Motion skip: still alone + warn"]
    src -->|"error"| err["Error → copy fails"]
    src -->|"ok"| chk["bulk-upload-check motion on t<br/>(lock per checksum)"]
    chk -->|"error"| err
    chk -->|"accept (missing)"| bud{"Fits SYNC_TMP_BUDGET?"}
    bud -->|"no"| err
    bud -->|"yes"| upm["Download + verify, upload HIDDEN"]
    upm -->|"created"| use["Use motion id"]
    upm -->|"duplicate"| ex
    upm -->|"error"| err
    chk -->|"exists"| ex["GET existing motion on t"]
    ex -->|"not found (unreadable)"| skip
    ex -->|"trashed"| tr{"SYNC_RESTORE_TRASHED and not bootstrap?"}
    tr -->|"no"| skip
    tr -->|"yes"| rs["Restore + re-GET"]
    rs -->|"still trashed"| skip
    rs -->|"ok"| vis
    ex -->|"not trashed"| vis{"Visibility hidden?"}
    vis -->|"no (timeline/archive)"| skip
    vis -->|"yes"| use
    use --> lk["Upload still with livePhotoVideoId,<br/>or PUT link on existing still"]
    lk --> na["Motion never added to an album"]
```

## 7. Metadata merge per field

Fields: description, date + UTC offset, location (lat/lon), rating, visibility (timeline|archive only).
Favorites are never synced. Baselines are per side and bound to the asset id.

```mermaid
flowchart TD
    f(["Field of a paired key"]) --> kb{"Baseline known on both sides<br/>(present and same asset id)?"}
    kb -->|"yes"| ch{"Which side changed vs its baseline?"}
    ch -->|"none"| nw["No write"]
    ch -->|"one side s"| wr["Write s value to other side"]
    ch -->|"both, values equal"| nw
    ch -->|"both, values differ"| cw["SYNC_CONFLICT_WINNER (default A) wins"]
    kb -->|"no"| eq{"Values equal?"}
    eq -->|"yes"| nw
    eq -->|"no"| kind{"Field"}
    kind -->|"description / rating / location"| fill["Fill the empty side only"]
    kind -->|"date / visibility"| div["No write, note diverged:field"]

    wr --> own{"Target owned by sync user?"}
    cw --> own
    fill --> own
    own -->|"no"| no["Skip: not_owner"]
    own -->|"yes"| clr{"Clearing location?"}
    clr -->|"yes"| uns["Skip: unsupported:clear_location"]
    clr -->|"no"| reget["Re-GET target"]
    reget -->|"changed since snapshot"| st["Skip: stale (next cycle)"]
    reget -->|"unchanged"| put["PUT changed fields only"]
    put -->|"ok"| base["Baseline = snapshot + read-back of written fields"]
    put -->|"error"| keep["Keep old baseline for unwritten changes, record failure"]
    nw --> rb["Record baselines"]
    div --> rb
```

Album info (name, description) uses the same rules with album-level baselines; without a baseline only an
empty description is filled, never a rename.

## 8. End of pair run: state and result

```mermaid
flowchart TD
    e(["After copy phase / cancellation"]) --> clean{"Not canceled, 0 item errors, 0 deferred,<br/>no guard tripped, no quota / too_large skips?"}
    clean -->|"yes"| bs["Bootstrap = false, LastSuccess = now"]
    clean -->|"no"| keepbs["Bootstrap unchanged"]
    bs --> gc["GC empty items"]
    keepbs --> gc
    gc --> save["Atomic save (tmp + fsync + rename + dir fsync), no context"]
    save -->|"error"| se(["Pair error: save state"])
    save -->|"ok"| cx{"Canceled?"}
    cx -->|"yes"| ce(["Return cancel (not a failure)"])
    cx -->|"no"| ok(["Summary: result ok/partial, copied, unlinked,<br/>meta writes, skipped by reason → /status, metrics"])
```
