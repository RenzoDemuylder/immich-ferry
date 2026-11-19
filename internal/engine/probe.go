package engine

import (
	"fmt"

	"immich-ferry/internal/engine/planner"
	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

type probeOutcome int

const (
	probeUnlink probeOutcome = iota
	probeHidden
	probeOffline
	probeVisible
)

// classifyProbe applies DESIGN step 5 to the probed members of a key.
func classifyProbe(members []immich.Asset) probeOutcome {
	var offline, hidden bool
	for _, m := range members {
		switch {
		case m.IsOffline:
			offline = true
		case m.IsTrashed:
			// gone
		case visible(m.Visibility):
			return probeVisible
		default:
			hidden = true
		}
	}
	switch {
	case offline:
		return probeOffline
	case hidden:
		return probeHidden
	}
	return probeUnlink
}

type removalCand struct {
	act  planner.Action
	ids  []string
	from Side // side probed (where the key vanished)
	to   Side // side to unlink from
}

// removalsPhase probes each candidate and unlinks confirmed removals.
func (r *run) removalsPhase(acts []planner.Action) {
	r.progress(PhaseRemovals, 0, len(acts))
	if len(acts) == 0 {
		return
	}
	var bySide [2][]planner.Action
	for _, a := range acts {
		bySide[a.From] = append(bySide[a.From], a)
	}
	done := 0
	for ps := A; ps <= B; ps++ {
		list := bySide[ps]
		if len(list) == 0 || r.ctx.Err() != nil {
			continue
		}
		var cands []removalCand
		sums := map[string]string{} // checksum -> key
		var all []string
		for _, a := range list {
			r.mu.Lock()
			it := r.ps.Items[string(a.Key)]
			var refs []state.AssetRef
			if it != nil {
				refs = append(refs, it.Refs[ps]...)
			}
			r.mu.Unlock()
			if len(refs) == 0 {
				r.log.Warn("no recorded refs on probe side; skipping removal candidate", "phase", "removals", "key", string(a.Key), "side", ps.String())
				done++
				continue
			}
			c := removalCand{act: a, from: ps, to: Side(a.To)}
			for _, ref := range a.Refs {
				c.ids = append(c.ids, ref.ID)
			}
			for _, ref := range refs {
				if _, dup := sums[ref.Checksum]; !dup {
					all = append(all, ref.Checksum)
				}
				sums[ref.Checksum] = string(a.Key)
			}
			cands = append(cands, c)
		}
		if len(cands) == 0 {
			continue
		}
		probed, err := r.e.cl[ps].ProbeAlbumAssets(r.ctx, r.albumIDs[ps], all)
		if err != nil {
			if r.canceled(err) {
				return
			}
			for _, c := range cands {
				r.recordFailure(string(c.act.Key), "probe", err, timeZero)
				r.itemError(string(c.act.Key), "probe", ps, err)
			}
			done += len(cands)
			r.progress(PhaseRemovals, done, len(acts))
			continue
		}
		byKey := map[string][]immich.Asset{}
		for _, a := range probed {
			if k, ok := sums[a.Checksum]; ok {
				byKey[k] = append(byKey[k], a)
			}
		}
		var unlink []removalCand
		for _, c := range cands {
			key := string(c.act.Key)
			switch classifyProbe(byKey[key]) {
			case probeUnlink:
				hidden := false
				if len(byKey[key]) == 0 {
					h, herr := r.hiddenElsewhere(ps, key)
					if herr != nil {
						// Unknown: fail safe, never unlink on an unverified probe.
						if !r.canceled(herr) {
							r.recordFailure(key, "probe", herr, timeZero)
							r.itemError(key, "probe", ps, herr)
						}
						done++
						continue
					}
					hidden = h
				}
				if hidden {
					r.mu.Lock()
					r.ps.Item(key).Blocked = &state.Blocked{Op: "probe", Side: uint8(ps), Reason: string(SkipServerHidden), Since: r.now()}
					r.mu.Unlock()
					r.addSkip(SkipServerHidden, 1)
					r.log.Info("removal candidate exists hidden outside the album on probe side (auto-linked motion); blocked", "phase", "removals", "key", key, "side", ps.String(), "op", "probe")
					done++
					continue
				}
				unlink = append(unlink, c)
			case probeHidden:
				r.mu.Lock()
				it := r.ps.Item(key)
				it.Blocked = &state.Blocked{Op: "probe", Side: uint8(ps), Reason: string(SkipServerHidden), Since: r.now()}
				r.mu.Unlock()
				r.addSkip(SkipServerHidden, 1)
				r.log.Info("removal candidate is hidden on probe side (likely auto-linked motion); blocked", "phase", "removals", "key", key, "side", ps.String(), "op", "probe")
				done++
			case probeOffline:
				r.addSkip(SkipOffline, 1)
				r.log.Info("removal candidate is offline on probe side; skipping", "phase", "removals", "key", key, "side", ps.String(), "op", "probe")
				done++
			case probeVisible:
				r.addSkip(SkipStale, 1)
				r.log.Info("removal candidate still visible on probe side (snapshot glitch); skipping", "phase", "removals", "key", key, "side", ps.String(), "op", "probe")
				done++
			}
		}
		r.progress(PhaseRemovals, done, len(acts))
		done += r.unlinkBatch(unlink, done, len(acts))
	}
}

var timeZero = zeroTime()

// unlinkBatch removes confirmed candidates (all from the same To side).
func (r *run) unlinkBatch(cands []removalCand, done0, total int) int {
	if len(cands) == 0 || r.ctx.Err() != nil {
		return 0
	}
	to := cands[0].to
	var ids []string
	for _, c := range cands {
		ids = append(ids, c.ids...)
	}
	res, err := r.e.cl[to].RemoveFromAlbum(r.ctx, r.albumIDs[to], ids)
	r.mut()
	if err != nil {
		if r.canceled(err) {
			return 0
		}
		for _, c := range cands {
			r.recordFailure(string(c.act.Key), "unlink", err, timeZero)
			r.itemError(string(c.act.Key), "unlink", to, err)
		}
		return len(cands)
	}
	byID := map[string]immich.BulkIDResult{}
	for _, x := range res {
		byID[x.ID] = x
	}
	for i, c := range cands {
		key := string(c.act.Key)
		ok, denied := true, false
		var bad string
		for _, id := range c.ids {
			x, found := byID[id]
			switch {
			case found && (x.Success || x.Error == "not_found"):
			case found && x.Error == "no_permission":
				denied, ok = true, false
			default:
				ok = false
				bad = x.Error
				if !found {
					bad = "missing result"
				}
			}
		}
		switch {
		case ok:
			r.mu.Lock()
			r.ps.Delete(key)
			dirInc(&r.sum.Unlinked, c.from)
			r.mu.Unlock()
			r.log.Info("unlinked", "phase", "removals", "key", key, "side", to.String(), "op", "unlink")
		case denied:
			r.mu.Lock()
			r.ps.Item(key).Blocked = &state.Blocked{Op: "unlink", Side: uint8(to), Reason: "no_permission", Since: r.now()}
			r.mu.Unlock()
			r.addSkip(SkipBlocked, 1)
		default:
			err := fmt.Errorf("unlink %s: %s", key, bad)
			r.recordFailure(key, "unlink", err, timeZero)
			r.itemError(key, "unlink", to, err)
		}
		r.tick()
		r.progress(PhaseRemovals, done0+i+1, total)
	}
	return len(cands)
}

// hiddenElsewhere reports whether the key still exists on side ps as a live,
// non-visible asset outside the album (the server hides auto-linked motion
// videos and removes them from all albums; that is not a user removal). A
// non-nil error means "unknown": the caller must not unlink.
func (r *run) hiddenElsewhere(ps Side, key string) (bool, error) {
	r.mu.Lock()
	var sums []immich.UploadCheckItem
	if it := r.ps.Items[key]; it != nil {
		for _, ref := range it.Refs[ps] {
			sums = append(sums, immich.UploadCheckItem{ID: ref.ID, Checksum: ref.Checksum})
		}
	}
	r.mu.Unlock()
	if len(sums) == 0 {
		return false, nil
	}
	res, err := r.e.cl[ps].UploadCheck(r.ctx, sums)
	if err != nil {
		return false, fmt.Errorf("probe upload check: %w", err)
	}
	for _, x := range res {
		if x.Accept || x.AssetID == "" || x.IsTrashed {
			continue
		}
		a, err := r.e.cl[ps].GetAsset(r.ctx, x.AssetID)
		if err != nil {
			if immich.KindOf(err) == immich.KindNotFound && r.ctx.Err() == nil {
				continue
			}
			return false, fmt.Errorf("probe get asset %s: %w", x.AssetID, err)
		}
		if !a.IsTrashed && !a.IsOffline && !visible(a.Visibility) {
			return true, nil
		}
	}
	return false, nil
}
