package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"immich-ferry/internal/engine/planner"
	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

// run holds the state of one RunPair/PlanPair invocation.
type run struct {
	e   *Engine
	o   Options
	ctx context.Context
	p   Pair
	log *slog.Logger

	mutating  bool // false for PlanPair and DryRun
	executed  bool // phases after planning started
	bootstrap bool

	mu       sync.Mutex // guards ps, sum, lastSave, sinceSave, consecTransient, abort
	ps       *state.PairState
	sum      RunSummary
	lastSave time.Time
	since    int

	userIDs  [2]string
	albumIDs [2]string
	snaps    [2]planner.Snapshot
	assets   [2]map[string]immich.Asset
	members  [2]map[string]planner.Member
	warnings []string
	// incomplete marks a side whose snapshot lacks assets (hash failures).
	incomplete [2]bool

	mutations atomic.Int64
	bytesDown atomic.Int64
	bytesUp   atomic.Int64
	budget    *byteBudget

	pmu       sync.Mutex // serializes progress callbacks
	phase     Phase
	pdone     int
	ptotal    int
	lastEmit  time.Time
	quota     [2]atomic.Bool
	quotaErr  [2]atomic.Bool
	motionMu  sync.Mutex
	motionLks map[string]*sync.Mutex

	consecTransient int
	abort           bool
	addQ            [2]addQueue
}

func (r *run) curPhase() Phase {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	return r.phase
}

func (r *run) progress(ph Phase, done, total int) {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	r.phase, r.pdone, r.ptotal = ph, done, total
	r.emitLocked()
}

func (r *run) emitLocked() {
	r.lastEmit = time.Now()
	if fn := r.o.OnProgress; fn != nil {
		fn(ProgressEvent{Pair: r.p.Name, Phase: r.phase, Done: r.pdone, Total: r.ptotal,
			Bytes: r.bytesDown.Load() + r.bytesUp.Load()})
	}
}

// byteTick emits a rate-limited progress event while bytes are flowing.
func (r *run) byteTick() {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	if time.Since(r.lastEmit) >= 500*time.Millisecond {
		r.emitLocked()
	}
}

func (r *run) mut() { r.mutations.Add(1) }

// RunPair runs one full cycle for p.
func (e *Engine) RunPair(ctx context.Context, p Pair) (RunSummary, error) {
	_, sum, err := e.execute(ctx, p, false)
	return sum, err
}

// PlanPair snapshots and plans without mutating anything.
func (e *Engine) PlanPair(ctx context.Context, p Pair) (planner.Plan, RunSummary, error) {
	return e.execute(ctx, p, true)
}

func (e *Engine) execute(ctx context.Context, p Pair, planOnly bool) (planner.Plan, RunSummary, error) {
	r := &run{e: e, o: e.o, ctx: ctx, p: p, mutating: !planOnly && !e.o.DryRun,
		log:       e.log.With("pair", p.Name),
		budget:    newByteBudget(e.o.TmpBudgetBytes),
		motionLks: map[string]*sync.Mutex{},
	}
	r.sum = RunSummary{Pair: p.Name, Started: e.o.Now(), Skipped: map[SkipReason]int{}}
	plan, err := r.cycle(planOnly)
	r.finalizeSummary(err)
	r.progress(PhaseDone, 0, 0)
	r.log.Info("cycle finished", "phase", "done", "result", string(r.sum.Result),
		"copied_ab", r.sum.Copied.AtoB, "copied_ba", r.sum.Copied.BtoA,
		"unlinked_ab", r.sum.Unlinked.AtoB, "unlinked_ba", r.sum.Unlinked.BtoA,
		"item_errors", r.sum.ItemErrorCount, "mutations", r.sum.Mutations, "deferred", r.sum.Deferred)
	return plan, r.sum, err
}

func (r *run) cycle(planOnly bool) (planner.Plan, error) {
	var plan planner.Plan
	e := r.e
	r.progress(PhasePreflight, 0, 1)
	e.mu.Lock()
	done := e.pfDone
	e.mu.Unlock()
	if !done {
		if err := e.Preflight(r.ctx); err != nil {
			return plan, err
		}
	}
	e.mu.Lock()
	r.userIDs = e.userIDs
	e.mu.Unlock()
	r.progress(PhasePreflight, 1, 1)

	// Markers.
	allowMass := r.o.AllowMassRemoval
	var approve bool
	if r.mutating {
		// A marker is removed only once the action it requests can no longer be lost:
		// a failed reset or an aborted cycle keeps it for the next run.
		reset, err := e.st.HasMarker(r.p.Name, "reset")
		if err != nil {
			return plan, fmt.Errorf("reset marker: %w", err)
		}
		if reset {
			r.log.Warn("reset marker found: discarding state", "op", "reset")
			if err := e.st.Reset(r.p.Name); err != nil {
				return plan, err
			}
			if _, err := e.st.TakeMarker(r.p.Name, "reset"); err != nil {
				return plan, fmt.Errorf("reset marker: %w", err)
			}
		}
		approve, err = e.st.HasMarker(r.p.Name, "approve-removals")
		if err != nil {
			return plan, fmt.Errorf("approve-removals marker: %w", err)
		}
		if approve {
			allowMass = true
		}
	}

	load := e.st.Load
	if !r.mutating {
		load = e.st.Peek // dry run / plan: never move files aside
	}
	ps, status, err := load(r.p.Name)
	if err != nil {
		return plan, fmt.Errorf("load state: %w", err)
	}
	if status == state.CorruptMovedAside {
		if r.mutating {
			r.log.Warn("state was corrupt and moved aside; starting fresh", "op", "load")
		} else {
			r.log.Warn("state is corrupt or has an unknown schema; planning from scratch (file left untouched)", "op", "load")
		}
	}
	r.ps = ps

	if err := r.resolveAlbums(); err != nil {
		return plan, err
	}
	fp := state.Fingerprint{UserIDs: r.userIDs, AlbumIDs: r.albumIDs}
	if r.mutating {
		ps, st2, err := e.st.Reconcile(r.p.Name, r.ps, fp)
		if err != nil {
			return plan, fmt.Errorf("reconcile state: %w", err)
		}
		if st2 == state.FingerprintReset {
			r.log.Warn("state fingerprint changed (user or album ids): starting fresh", "op", "reconcile")
		}
		r.ps = ps
	} else if r.ps.Fingerprint != (state.Fingerprint{}) && r.ps.Fingerprint != fp {
		r.ps = state.NewPairState(r.p.Name)
	}
	r.ps.Fingerprint = fp
	r.ps.AlbumIDs = r.albumIDs
	r.ps.Schema = state.Schema
	r.bootstrap = r.ps.Bootstrap

	for s := A; s <= B; s++ {
		if err := r.snapshotSide(s); err != nil {
			return plan, err
		}
	}
	r.gcHashes()
	r.recoverIntents()

	r.progress(PhasePlan, 0, 1)
	popts := planner.Options{
		PropagateRemovals: r.o.PropagateRemovals, MaxRemovalRatio: r.o.MaxRemovalRatio, MaxRemovalsMin: r.o.MaxRemovalsMin,
		AllowMassRemoval: allowMass, ConflictWinner: uint8(r.o.ConflictWinner), SyncMetadata: r.o.SyncMetadata,
		SyncAlbumInfo: r.o.SyncAlbumInfo, Bootstrap: r.bootstrap, BlockedRecheck: r.o.BlockedRecheck,
		BackoffBase: r.o.BackoffBase, BackoffMax: r.o.BackoffMax, PendingMetaTimeout: r.o.PendingMetaTimeout,
	}
	plan = planner.Build(r.snaps[0], r.snaps[1], r.ps, popts, r.now())
	r.filterIncomplete(&plan)
	for _, w := range plan.Warnings {
		r.log.Warn(w, "phase", "plan")
	}
	r.sum.GuardTripped = plan.GuardTripped
	for k, n := range plan.Skipped {
		r.sum.Skipped[SkipReason(k)] += n
	}
	r.progress(PhasePlan, 1, 1)

	if !r.mutating {
		r.countPlanned(plan)
		return plan, nil
	}

	if approve {
		// Consumed right before execution: one-shot, but not lost to a failed load/snapshot.
		if _, err := e.st.TakeMarker(r.p.Name, "approve-removals"); err != nil {
			return plan, fmt.Errorf("approve-removals marker: %w", err)
		}
		r.log.Warn("approve-removals marker consumed: mass removal allowed this run", "op", "approve-removals")
	}
	r.executed = true
	r.applyPaired(plan)
	if err := r.execute2(plan); err != nil {
		return plan, err
	}
	return plan, nil
}

// filterIncomplete removes copies into, drops for and removals probing a side
// whose snapshot is incomplete (an external asset could not be hashed), so a
// missing key is never mistaken for an absent asset.
func (r *run) filterIncomplete(plan *planner.Plan) {
	if !r.incomplete[0] && !r.incomplete[1] {
		return
	}
	var copies, removals []planner.Action
	skipped := 0
	for _, a := range plan.Copies {
		if r.incomplete[a.To] {
			skipped++
			continue
		}
		copies = append(copies, a)
	}
	for _, a := range plan.Removals {
		if r.incomplete[a.From] {
			skipped++
			continue
		}
		removals = append(removals, a)
	}
	skipped += len(plan.Drops)
	plan.Copies, plan.Removals, plan.Drops = copies, removals, nil
	if skipped > 0 {
		plan.Skipped[string(SkipSnapshotIncomplete)] += skipped
		r.log.Warn("snapshot incomplete (asset hash failures); skipping copies into and removals for the affected side this cycle",
			"phase", "plan", "incomplete_a", r.incomplete[0], "incomplete_b", r.incomplete[1], "skipped", skipped)
	}
}

// execute2 runs the mutating phases and the final save.
func (r *run) execute2(plan planner.Plan) error {
	r.removalsPhase(plan.Removals)
	r.checkpointNow("removals")
	if r.ctx.Err() == nil {
		r.metadataPhase(plan.Meta)
		r.checkpointNow("metadata")
	}
	if r.ctx.Err() == nil {
		r.albumInfoPhase(plan.AlbumInfo)
		r.checkpointNow("albuminfo")
	}
	if r.ctx.Err() == nil {
		r.copyPhase(plan.Copies)
	} else {
		r.mu.Lock()
		r.sum.Deferred += len(plan.Copies)
		r.mu.Unlock()
	}

	cancelErr := r.ctx.Err()
	r.mu.Lock()
	clean := cancelErr == nil && r.sum.ItemErrorCount == 0 && r.sum.Deferred == 0 &&
		!r.sum.GuardTripped[0] && !r.sum.GuardTripped[1] &&
		r.sum.Skipped[SkipQuota] == 0 && r.sum.Skipped[SkipTooLarge] == 0
	if clean {
		r.ps.Bootstrap = false
		r.ps.LastSuccess = r.now()
	}
	r.gcItemsLocked()
	err := r.e.st.Save(r.p.Name, r.ps)
	r.mu.Unlock()
	r.progress(PhaseCheckpoint, 1, 1)
	if err != nil {
		r.log.Error("final state save failed", "phase", "checkpoint", "err", err)
		return fmt.Errorf("save state: %w", err)
	}
	if cancelErr != nil {
		return cancelErr
	}
	return nil
}

// gcItemsLocked removes empty items. Caller holds r.mu.
func (r *run) gcItemsLocked() {
	for k, it := range r.ps.Items {
		if it == nil || (!it.InBoth && it.Intent == nil && it.Pending == nil && it.Blocked == nil && it.Failure == nil) {
			delete(r.ps.Items, k)
		}
	}
}

// saveLocked persists state. Caller holds r.mu.
func (r *run) saveLocked() {
	if err := r.e.st.Save(r.p.Name, r.ps); err != nil {
		r.log.Error("state save failed", "phase", "checkpoint", "err", err)
	}
	r.lastSave = r.now()
	r.since = 0
}

// checkpointNow saves at a phase end.
func (r *run) checkpointNow(phase string) {
	r.mu.Lock()
	r.saveLocked()
	r.mu.Unlock()
	r.log.Debug("checkpoint", "phase", "checkpoint", "after", phase)
}

// tick counts a completed action and saves if both checkpoint thresholds are met.
func (r *run) tick() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.since++
	if r.since >= r.o.CheckpointEvery && r.now().Sub(r.lastSave) >= r.o.CheckpointInterval {
		r.saveLocked()
	}
}

// recoverIntents turns a leftover write-ahead Intent for a key that is now in
// both albums into pending state (the earlier run crashed after uploading).
func (r *run) recoverIntents() {
	for k, it := range r.ps.Items {
		if it == nil || it.Intent == nil {
			continue
		}
		key := planner.Key(k)
		ga, gb := r.snaps[0].Groups[key], r.snaps[1].Groups[key]
		if len(ga) == 0 || len(gb) == 0 {
			continue
		}
		in := it.Intent
		// Without the UploadStarted marker the target asset predates our copy
		// (reused duplicate): it is the user's own and must not be overwritten.
		if it.Pending == nil && in.To <= 1 && in.UploadStarted {
			g := r.snaps[in.To].Groups[key]
			it.Pending = &state.Pending{To: in.To, TargetID: g[0].Ref.ID, Since: in.Started}
			it.Ours[in.To] = g[0].Ref.ID
			sm := in.SourceMeta
			src, tgt := cloneMetaT(sm), cloneMetaT(sm)
			it.Meta[1-in.To], it.MetaID[1-in.To] = &src, r.snaps[1-in.To].Groups[key][0].Ref.ID
			it.Meta[in.To], it.MetaID[in.To] = &tgt, g[0].Ref.ID
		}
		it.Intent = nil
	}
}

func cloneMetaT(m state.Meta) state.Meta {
	c := m
	if m.TakenAt != nil {
		v := *m.TakenAt
		c.TakenAt = &v
	}
	if m.OffsetSec != nil {
		v := *m.OffsetSec
		c.OffsetSec = &v
	}
	if m.Lat != nil {
		v := *m.Lat
		c.Lat = &v
	}
	if m.Lon != nil {
		v := *m.Lon
		c.Lon = &v
	}
	if m.Rating != nil {
		v := *m.Rating
		c.Rating = &v
	}
	return c
}

// applyPaired and drops: bookkeeping that needs no API calls.
func (r *run) applyPaired(plan planner.Plan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range plan.Paired {
		it := r.ps.Item(string(k))
		it.InBoth = true
		it.Blocked = nil
		it.Failure = nil
		it.Intent = nil
		for s := 0; s < 2; s++ {
			it.Refs[s] = refsOf(r.snaps[s].Groups[k])
		}
	}
	for _, k := range plan.Drops {
		r.ps.Delete(string(k))
	}
}

func refsOf(ms []planner.Member) []state.AssetRef {
	out := make([]state.AssetRef, len(ms))
	for i, m := range ms {
		out[i] = m.Ref
	}
	return out
}

// countPlanned fills the summary from a plan without executing it.
func (r *run) countPlanned(plan planner.Plan) {
	for _, a := range plan.Copies {
		dirInc(&r.sum.Copied, Side(a.From))
	}
	for _, a := range plan.Removals {
		dirInc(&r.sum.Unlinked, Side(a.From))
	}
	for _, a := range plan.Meta {
		switch a.Kind {
		case planner.ActionLinkRepair:
			dirInc(&r.sum.Linked, Side(a.From))
		default:
			for s := 0; s < 2; s++ {
				if a.Updates[s] != nil {
					dirInc(&r.sum.MetaUpdates, Side(1-s))
				}
			}
		}
	}
	for _, a := range plan.AlbumInfo {
		for s := 0; s < 2; s++ {
			if a.AlbumUpdates[s] != nil {
				dirInc(&r.sum.AlbumInfoUpdated, Side(1-s))
			}
		}
	}
	r.sum.Pending = plan.Pending
}

// resolveAlbums resolves/creates both albums (Appendix D order).
func (r *run) resolveAlbums() error {
	r.progress(PhaseSnapshot, 0, 2)
	for s := A; s <= B; s++ {
		id, err := r.resolveAlbum(s)
		if err != nil {
			return err
		}
		r.albumIDs[s] = id
	}
	return r.claimAlbums()
}

// claimAlbums rejects two pairs that resolve to the same album: they would
// fight over it.
func (r *run) claimAlbums() error {
	e := r.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resolved == nil {
		e.resolved = map[resolvedAlbum]string{}
	}
	var claims []resolvedAlbum
	for s := A; s <= B; s++ {
		if r.albumIDs[s] == "" {
			continue
		}
		c := resolvedAlbum{s, r.albumIDs[s]}
		if owner, ok := e.resolved[c]; ok && owner != r.p.Name {
			return fmt.Errorf("%w: pairs %q and %q both resolve to album %s on side %s", ErrConfig, owner, r.p.Name, c.id, s)
		}
		claims = append(claims, c)
	}
	for k, v := range e.resolved {
		if v == r.p.Name {
			delete(e.resolved, k)
		}
	}
	for _, c := range claims {
		e.resolved[c] = r.p.Name
	}
	return nil
}

func (r *run) resolveAlbum(s Side) (string, error) {
	ref := r.p.A
	if s == B {
		ref = r.p.B
	}
	cl := r.e.cl[s]
	id := ref.ID
	if id == "" {
		return "", fmt.Errorf("%w: pair %s side %s: no album id configured", ErrConfig, r.p.Name, s)
	}
	al, err := cl.GetAlbum(r.ctx, id)
	if err != nil {
		if immich.KindOf(err) == immich.KindNotFound && r.ctx.Err() == nil {
			return "", fmt.Errorf("%w: album %s gone on side %s; refusing to recreate", ErrConfig, id, s)
		}
		return "", fmt.Errorf("get album %s on side %s: %w", id, s, err)
	}
	role := roleOf(al, r.userIDs[s])
	if !canEdit(role) {
		return "", fmt.Errorf("%w: album %s on side %s: need owner or editor role, have %q", ErrConfig, id, s, role)
	}
	return id, nil
}

// roleOf returns the album role of user ("owner", "editor", "viewer" or "").
func roleOf(al immich.Album, user string) string {
	role := ""
	for _, u := range al.Users {
		if u.UserID == user && (role == "" || u.Role == "owner") {
			role = u.Role
		}
	}
	return role
}

// canEdit reports whether role may add and remove album assets.
func canEdit(role string) bool { return role == "owner" || role == "editor" }

var _ = errors.New
