package engine

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"immich-ferry/internal/engine/planner"
	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

const (
	copyBatchSize        = 100
	albumFlushThreshold  = 100
	maxConsecTransient   = 5
	copyKeyPlaceholderOp = "copy"
	tooLargeBackoff      = 30 * 24 * time.Hour
)

type copyTask struct {
	act        planner.Action
	key        string
	from, to   Side
	src        immich.Asset
	mem        planner.Member
	prevIntent *state.Intent
	hadIntent  bool
	uploaded   bool
}

// copyDone is a copied asset waiting for its album add.
type copyDone struct {
	t          *copyTask
	targetID   string
	targetSum  string
	created    bool
	intentHint bool
}

type addQueue struct {
	mu    sync.Mutex
	items []*copyDone
}

func (r *run) shouldStop(deadlineExceeded func() bool) bool {
	if r.ctx.Err() != nil || deadlineExceeded() {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.abort
}

// copyPhase copies the planned assets within the time budget.
func (r *run) copyPhase(acts []planner.Action) {
	total := len(acts)
	r.progress(PhaseCopy, 0, total)
	if total == 0 {
		return
	}
	deadline := r.now().Add(r.o.CopyPhaseBudget)
	expired := func() bool { return !r.now().Before(deadline) }
	done := 0
	rest := acts
	for len(rest) > 0 && !r.shouldStop(expired) {
		n := min(copyBatchSize, len(rest))
		batch := rest[:n]
		rest = rest[n:]
		r.runBatch(batch, expired, &done, total)
	}
	if len(rest) > 0 {
		r.count(func(s *RunSummary) { s.Deferred += len(rest) })
		r.log.Info("copy phase stopped; remaining copies deferred", "phase", "copy", "deferred", len(rest))
	}
	for s := A; s <= B; s++ {
		r.flushAdds(s)
	}
	r.checkpointNow("copy")
}

func (r *run) runBatch(batch []planner.Action, expired func() bool, done *int, total int) {
	var tasks []*copyTask
	r.mu.Lock()
	for _, a := range batch {
		t := &copyTask{act: a, key: string(a.Key), from: Side(a.From), to: Side(a.To)}
		if len(a.Refs) == 0 {
			continue
		}
		src, ok := r.assets[t.from][a.Refs[0].ID]
		mem, ok2 := r.members[t.from][a.Refs[0].ID]
		if !ok || !ok2 {
			continue
		}
		t.src, t.mem = src, mem
		it := r.ps.Item(t.key)
		t.prevIntent = it.Intent
		// A leftover intent only means "the earlier upload is ours" if it was
		// persisted right before an upload actually started (UploadStarted).
		t.hadIntent = it.Intent != nil && it.Intent.To == uint8(t.to) && it.Intent.UploadStarted
		tasks = append(tasks, t)
	}
	r.mu.Unlock()

	// Upload checks per target side.
	checks := map[string]immich.UploadCheckResult{}
	var live []*copyTask
	for s := A; s <= B; s++ {
		var items []immich.UploadCheckItem
		var ts []*copyTask
		for _, t := range tasks {
			if t.to == s {
				items = append(items, immich.UploadCheckItem{ID: t.key, Checksum: t.key})
				ts = append(ts, t)
			}
		}
		if len(items) == 0 {
			continue
		}
		res, err := r.e.cl[s].UploadCheck(r.ctx, items)
		if err != nil {
			for _, t := range ts {
				r.failTask(t, "uploadcheck", err)
				*done++
			}
			continue
		}
		for _, x := range res {
			checks[x.ID] = x
		}
		live = append(live, ts...)
	}

	ch := make(chan *copyTask)
	var wg sync.WaitGroup
	var dmu sync.Mutex
	for i := 0; i < min(r.o.Concurrency, max(len(live), 1)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				if r.shouldStop(expired) {
					r.deferTask(t)
				} else if x, ok := checks[t.key]; !ok {
					r.failTask(t, "uploadcheck", fmt.Errorf("upload check returned no result for %s", t.key))
				} else {
					r.copyOne(t, x)
				}
				dmu.Lock()
				*done++
				d := *done
				dmu.Unlock()
				r.tick()
				r.progress(PhaseCopy, d, total)
			}
		}()
	}
	for _, t := range live {
		ch <- t
	}
	close(ch)
	wg.Wait()
	for s := A; s <= B; s++ {
		r.flushAdds(s)
	}
	r.checkpointNow("copy-batch")
}

// deferTask undoes the write-ahead for a task that never started.
func (r *run) deferTask(t *copyTask) {
	r.mu.Lock()
	r.ps.Item(t.key).Intent = t.prevIntent
	r.sum.Deferred++
	r.mu.Unlock()
}

func (r *run) restoreIntent(t *copyTask) {
	if t.uploaded {
		return // an upload may have reached the server: keep the write-ahead record
	}
	r.mu.Lock()
	r.ps.Item(t.key).Intent = t.prevIntent
	r.mu.Unlock()
}

// block records a Blocked marker for the key.
func (r *run) block(t *copyTask, reason SkipReason) {
	r.mu.Lock()
	it := r.ps.Item(t.key)
	it.Blocked = &state.Blocked{Op: "copy", Side: uint8(t.to), Reason: string(reason), Since: r.now()}
	it.Intent = t.prevIntent
	if t.uploaded {
		it.Intent = &state.Intent{To: uint8(t.to), Started: r.now(), Source: t.mem.Ref, SourceMeta: cloneMetaT(t.mem.Meta), UploadStarted: true}
	}
	r.consecTransient = 0
	r.mu.Unlock()
	r.addSkip(reason, 1)
	r.log.Info("copy blocked", "phase", "copy", "key", t.key, "side", t.to.String(), "op", "copy", "reason", string(reason))
}

// failTask records a failed copy.
func (r *run) failTask(t *copyTask, op string, err error) {
	if r.canceled(err) {
		r.restoreIntent(t)
		return
	}
	kind := immich.KindOf(err)
	switch kind {
	case immich.KindQuota:
		first := !r.quotaErr[t.to].Swap(true)
		r.quota[t.to].Store(true)
		r.restoreIntent(t)
		r.addSkip(SkipQuota, 1)
		if first {
			r.itemError(t.key, op, t.to, err)
			r.log.Warn("quota exceeded; stopping uploads to this side for this run", "phase", "copy", "side", t.to.String(), "op", op)
		}
		return
	}
	if kind == immich.KindTooLarge || errors.Is(err, errTmpBudget) {
		// Retrying daily would re-transfer gigabytes every day: back off for a month.
		r.recordFailure(t.key, copyKeyPlaceholderOp, err, r.now().Add(tooLargeBackoff))
		r.addSkip(SkipTooLarge, 1)
		r.itemError(t.key, op, t.to, err)
		r.restoreIntent(t)
		if errors.Is(err, errTmpBudget) {
			r.log.Warn("asset exceeds the temp budget; skipping for 30 days. Raise SYNC_TMP_BUDGET / tmp.sizeLimit (and restart) to copy it, then run `immich-ferry state reset` to clear the backoff",
				"phase", "copy", "key", t.key, "side", t.to.String(), "op", op)
		} else {
			r.log.Warn("target rejected the asset as too large; skipping for 30 days (raise the server upload limit, then run `immich-ferry state reset` to force a retry)",
				"phase", "copy", "key", t.key, "side", t.to.String(), "op", op)
		}
		return
	}
	r.recordFailure(t.key, copyKeyPlaceholderOp, err, timeZero)
	r.itemError(t.key, op, t.to, err)
	r.restoreIntent(t)
	r.mu.Lock()
	if kind == immich.KindTransient {
		r.consecTransient++
		if r.consecTransient >= maxConsecTransient && !r.abort {
			r.abort = true
			r.log.Error("too many consecutive transient errors; aborting copy phase", "phase", "copy", "count", r.consecTransient)
		}
	} else {
		r.consecTransient = 0
	}
	r.mu.Unlock()
}

func (r *run) copyOne(t *copyTask, chk immich.UploadCheckResult) {
	if r.quota[t.to].Load() {
		r.addSkip(SkipQuota, 1)
		r.restoreIntent(t)
		return
	}
	if !chk.Accept {
		r.copyExisting(t, chk.AssetID)
		return
	}
	r.copyNew(t)
}

// copyExisting handles a target that already holds the content.
func (r *run) copyExisting(t *copyTask, id string) {
	cl := r.e.cl[t.to]
	a, err := cl.GetAsset(r.ctx, id)
	if err != nil {
		if immich.KindOf(err) == immich.KindNotFound && r.ctx.Err() == nil {
			// listed by upload check but unreadable: locked/hidden on the target.
			r.block(t, SkipTargetHidden)
			return
		}
		r.failTask(t, "get_existing", err)
		return
	}
	if !visible(a.Visibility) {
		r.block(t, SkipTargetHidden)
		return
	}
	if a.IsTrashed {
		if !r.o.RestoreTrashed || r.bootstrap {
			r.block(t, SkipTargetTrash)
			return
		}
		err := cl.RestoreFromTrash(r.ctx, []string{id})
		r.mut()
		if err != nil {
			r.failTask(t, "restore", err)
			return
		}
		a, err = cl.GetAsset(r.ctx, id)
		if err != nil {
			r.failTask(t, "restore", err)
			return
		}
		if a.IsTrashed {
			r.block(t, SkipTargetTrash)
			return
		}
		r.count(func(s *RunSummary) { dirInc(&s.Restored, t.from) })
	}
	if t.src.Type == "IMAGE" && t.mem.LivePhotoVideoID != "" && (a.LivePhotoVideoID == nil || *a.LivePhotoVideoID == "") &&
		a.OwnerID == r.userIDs[t.to] {
		err := r.linkLive(t.from, t.to, t.mem.LivePhotoVideoID, a.ID)
		switch {
		case err == nil:
			r.count(func(s *RunSummary) { dirInc(&s.Linked, t.from) })
		case isMotionSkip(err):
			r.log.Warn("copying still without live photo link", "phase", "copy", "key", t.key, "side", t.to.String(), "op", "link", "reason", err.Error())
		default:
			r.failTask(t, "link", err)
			return
		}
	}
	r.queueAdd(&copyDone{t: t, targetID: a.ID, targetSum: a.Checksum, intentHint: t.hadIntent})
}

// copyNew downloads, verifies and uploads an asset the target lacks.
func (r *run) copyNew(t *copyTask) {
	if err := r.checkTmpBudget(t.src); err != nil {
		r.failTask(t, "download", err)
		return
	}
	// Live photo: the motion goes first (hidden) and is never added to an album.
	motionID := ""
	if t.src.Type == "IMAGE" && t.mem.LivePhotoVideoID != "" {
		mid, err := r.ensureMotion(t.from, t.to, t.mem.LivePhotoVideoID)
		switch {
		case err == nil:
			motionID = mid
		case isMotionSkip(err):
			r.log.Warn("copying still without its motion video", "phase", "copy", "key", t.key, "side", t.to.String(), "op", "live_photo", "reason", err.Error())
		default:
			r.failTask(t, "motion", err)
			return
		}
	}
	release, err := r.budget.acquire(r.ctx, assetSize(t.src))
	if err != nil {
		r.failTask(t, "budget", err)
		return
	}
	defer release()
	// The key is the content SHA1 for both kinds of source, so always verify.
	tf, err := r.fetch(t.from, t.src, t.key, true)
	if err != nil {
		r.failTask(t, "download", err)
		return
	}
	defer tf.cleanup()

	vis := t.src.Visibility
	if !visible(vis) {
		vis = immich.VisibilityTimeline
	}
	// Write-ahead: persisted only for tasks that really upload, right before the
	// upload, so a leftover intent can only refer to an upload we started.
	t.uploaded = true
	r.mu.Lock()
	r.ps.Item(t.key).Intent = &state.Intent{To: uint8(t.to), Started: r.now(), Source: t.mem.Ref,
		SourceMeta: cloneMetaT(t.mem.Meta), UploadStarted: true}
	r.saveLocked()
	r.mu.Unlock()
	res, err := r.upload(t.to, t.src, tf, uploadOpts{visibility: vis, liveID: motionID})
	if err != nil {
		r.failTask(t, "upload", err)
		return
	}
	if res.Duplicate {
		// UploadCheck said "accept" moments ago, so a duplicate here is our own
		// upload that already reached the server (e.g. a retry after a proxy
		// timeout): treat it as ours so Pending/source-wins metadata applies.
		t.hadIntent = true
		r.copyExisting(t, res.ID)
		return
	}
	r.queueAdd(&copyDone{t: t, targetID: res.ID, targetSum: tf.sum, created: true})
}

func (r *run) queueAdd(d *copyDone) {
	q := &r.addQ[d.t.to]
	q.mu.Lock()
	q.items = append(q.items, d)
	n := len(q.items)
	q.mu.Unlock()
	if n >= albumFlushThreshold {
		r.flushAdds(d.t.to)
	}
}

// flushAdds adds the queued assets to the target album and finalizes state.
func (r *run) flushAdds(s Side) {
	q := &r.addQ[s]
	q.mu.Lock()
	items := q.items
	q.items = nil
	q.mu.Unlock()
	if len(items) == 0 {
		return
	}
	ctx := r.ctx
	cancel := func() {}
	if ctx.Err() != nil {
		// Finish what already reached the server so the next run need not redo it.
		ctx, cancel = contextForFlush()
	}
	defer cancel()
	ids := make([]string, len(items))
	for i, d := range items {
		ids[i] = d.targetID
	}
	res, err := r.e.cl[s].AddToAlbum(ctx, r.albumIDs[s], ids)
	r.mut()
	if err != nil {
		for _, d := range items {
			r.failTask(d.t, "album_add", err)
		}
		return
	}
	byID := map[string]immich.BulkIDResult{}
	for _, x := range res {
		byID[x.ID] = x
	}
	for _, d := range items {
		x, ok := byID[d.targetID]
		switch {
		case ok && (x.Success || x.Error == "duplicate"):
			r.completeCopy(d)
		default:
			msg := "missing result"
			kind := immich.KindUnknown
			if ok {
				msg = x.Error
				switch x.Error {
				case "no_permission":
					kind = immich.KindForbidden
				case "not_found":
					kind = immich.KindNotFound
				}
			}
			r.failTask(d.t, "album_add", immich.WithKind(kind, errors.New("album add: "+msg)))
		}
	}
}

// completeCopy records a successful copy in the state.
func (r *run) completeCopy(d *copyDone) {
	t := d.t
	r.mu.Lock()
	defer r.mu.Unlock()
	it := r.ps.Item(t.key)
	it.InBoth = true
	it.Refs[t.from] = refsOf(r.snaps[t.from].Groups[planner.Key(t.key)])
	it.Refs[t.to] = []state.AssetRef{{ID: d.targetID, Checksum: d.targetSum}}
	it.Intent, it.Failure, it.Blocked = nil, nil, nil
	if d.created || d.intentHint {
		it.Ours[t.to] = d.targetID
		now := r.now()
		it.Pending = &state.Pending{To: uint8(t.to), TargetID: d.targetID, Since: now}
		src := cloneMetaT(t.mem.Meta)
		tgt := cloneMetaT(t.mem.Meta)
		vis := t.src.Visibility
		if !visible(vis) {
			vis = immich.VisibilityTimeline
		}
		tgt.Visibility = string(vis)
		it.Meta[t.from], it.MetaID[t.from] = &src, t.mem.Ref.ID
		it.Meta[t.to], it.MetaID[t.to] = &tgt, d.targetID
	}
	r.consecTransient = 0
	dirInc(&r.sum.Copied, t.from)
	r.log.Info("copied", "phase", "copy", "key", t.key, "side", t.to.String(), "asset_id", d.targetID, "op", "copy", "created", d.created)
}
