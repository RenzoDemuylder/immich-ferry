package planner

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

func refsOf(ms []Member) []state.AssetRef {
	out := make([]state.AssetRef, len(ms))
	for i, m := range ms {
		out[i] = m.Ref
	}
	return out
}

// Build computes the plan for one cycle from both snapshots and the pair state.
// It never mutates st.
func Build(a, b Snapshot, st *state.PairState, o Options, now time.Time) Plan {
	plan := Plan{Skipped: map[string]int{}}
	snaps := [2]Snapshot{a, b}

	keySet := map[Key]struct{}{}
	for _, s := range snaps {
		for k := range s.Groups {
			keySet[k] = struct{}{}
		}
	}
	items := map[string]*state.Item{}
	if st != nil {
		items = st.Items
		for k, it := range items {
			if it != nil && it.InBoth {
				keySet[Key(k)] = struct{}{}
			}
		}
	}
	keys := make([]Key, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	var removals []Action
	for _, k := range keys {
		ga, gb := a.Groups[k], b.Groups[k]
		inA, inB := len(ga) > 0, len(gb) > 0
		it := items[string(k)]
		inS := it != nil && it.InBoth

		if it != nil && it.Blocked != nil && !(inA && inB) && (inA || inB) {
			expired := o.BlockedRecheck > 0 && now.Sub(it.Blocked.Since) > o.BlockedRecheck
			if !expired {
				plan.Skipped["blocked"]++
				continue
			}
		}

		switch {
		case inA && inB:
			plan.Paired = append(plan.Paired, k)
			buildPaired(&plan, k, [2][]Member{ga, gb}, it, o, now)
		case inA || inB:
			from, to := uint8(0), uint8(1)
			src := ga
			if !inA {
				from, to, src = 1, 0, gb
			}
			if !inS {
				if it != nil && it.Failure != nil && it.Failure.Next.After(now) {
					plan.Skipped["backoff"]++
					continue
				}
				plan.Copies = append(plan.Copies, Action{Kind: ActionCopy, Key: k, From: from, To: to, Refs: refsOf(src)})
				continue
			}
			// In S and now only on one side: the other side lost it.
			if !o.PropagateRemovals {
				continue
			}
			// src side still has it; the other side (to) lost it: probe `to`, unlink from `from`.
			probe, unlink := to, from
			plan.RemovalCandidates[unlink]++
			if it.Failure != nil && (it.Failure.Op == "unlink" || it.Failure.Op == "probe") && it.Failure.Next.After(now) {
				plan.Skipped["backoff"]++
				continue
			}
			removals = append(removals, Action{Kind: ActionProbeUnlink, Key: k, From: probe, To: unlink, Refs: refsOf(src)})
		default:
			if inS {
				plan.Drops = append(plan.Drops, k)
			}
		}
	}

	// Guard.
	nS := 0
	if st != nil {
		nS = st.InBothCount()
	}
	threshold := int(math.Ceil(o.MaxRemovalRatio * float64(nS)))
	if o.MaxRemovalsMin > threshold {
		threshold = o.MaxRemovalsMin
	}
	tripped := false
	if !o.AllowMassRemoval {
		for s := 0; s < 2; s++ {
			if plan.RemovalCandidates[s] > threshold {
				plan.GuardTripped[s] = true
				tripped = true
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("removal guard tripped for side %s: %d candidates > threshold %d (|S|=%d)",
					strings.ToUpper(sideName(s)), plan.RemovalCandidates[s], threshold, nS))
			}
		}
	}
	if tripped {
		plan.Skipped["guard"] += len(removals)
		removals = nil
	}
	plan.Removals = removals

	if o.SyncAlbumInfo {
		if act, ok := buildAlbumInfo(snaps, st, o); ok {
			plan.AlbumInfo = append(plan.AlbumInfo, act)
		}
	}
	return plan
}

func buildPaired(plan *Plan, k Key, groups [2][]Member, it *state.Item, o Options, now time.Time) {
	prim := [2]Member{groups[0][0], groups[1][0]}

	// Pending handling.
	pendingTarget := -1
	if it != nil && it.Pending != nil {
		t := int(it.Pending.To)
		if t == 0 || t == 1 {
			if !prim[t].Extracted && now.Sub(it.Pending.Since) < o.PendingMetaTimeout {
				plan.Pending++
				return
			}
			pendingTarget = t
		}
	}

	var out Action
	out.Kind = ActionMeta
	out.Key = k
	emit := false

	if o.SyncMetadata {
		cur := [2]*state.Meta{&prim[0].Meta, &prim[1].Meta}
		owned := [2]bool{prim[0].Owned, prim[1].Owned}
		var base [2]*state.Meta
		var noBase [2]bool
		for s := 0; s < 2; s++ {
			if it != nil {
				base[s] = it.Meta[s]
			}
			noBase[s] = base[s] == nil || it.MetaID[s] != prim[s].Ref.ID
		}
		use := func(*field) bool { return true }
		if pendingTarget >= 0 {
			use = func(f *field) bool { return !f.exif }
		}
		upd, newBase, conflicts := mergeFields(cur, base, owned, o.ConflictWinner, noBase, use)
		clear := false
		if pendingTarget >= 0 {
			t := pendingTarget
			wrote, notes := sourceWinsExif(cur, owned, 1-t, t, &upd, &newBase)
			conflicts = append(conflicts, notes...)
			clear = !wrote
		}
		out.Updates = upd
		out.NewBase = newBase
		out.NewBaseIDs = [2]string{prim[0].Ref.ID, prim[1].Ref.ID}
		out.TargetIDs = out.NewBaseIDs
		out.ClearPending = clear
		out.Reason = strings.Join(conflicts, ",")
		emit = clear || upd[0] != nil || upd[1] != nil
		for s := 0; s < 2 && !emit; s++ {
			if it == nil || noBase[s] || !EqualMeta(newBase[s], base[s]) {
				emit = true
			}
		}
		notable := false
		for _, c := range conflicts {
			if strings.HasPrefix(c, "not_owner:") {
				plan.Skipped["not_owner"]++
			} else if strings.HasPrefix(c, "diverged:") || strings.HasPrefix(c, "unsupported:") {
				notable = true
			}
		}
		if notable {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("key %s: %s", k, out.Reason))
		}
	} else if pendingTarget >= 0 {
		out.ClearPending = true
		emit = true
	}
	if emit {
		plan.Meta = append(plan.Meta, out)
	}

	// Link repair.
	for s := 0; s < 2; s++ {
		t := 1 - s
		if prim[s].LivePhotoVideoID != "" && prim[t].LivePhotoVideoID == "" && prim[t].Owned {
			plan.Meta = append(plan.Meta, Action{
				Kind: ActionLinkRepair, Key: k, From: uint8(s), To: uint8(t),
				Refs: []state.AssetRef{prim[s].Ref}, TargetID: prim[t].Ref.ID,
			})
			break
		}
	}
}

func buildAlbumInfo(snaps [2]Snapshot, st *state.PairState, o Options) (Action, bool) {
	cur := [2]state.AlbumInfo{
		{Name: snaps[0].AlbumName, Description: snaps[0].AlbumDescription},
		{Name: snaps[1].AlbumName, Description: snaps[1].AlbumDescription},
	}
	var base [2]*state.AlbumInfo
	if st != nil {
		base = st.AlbumInfo
	}
	known := base[0] != nil && base[1] != nil
	newBase := cur
	var upds [2]*immich.AlbumInfoUpdate

	type af struct {
		name string
		get  func(*state.AlbumInfo) *string
		fill bool
		set  func(*immich.AlbumInfoUpdate, string)
	}
	afs := []af{
		{"name", func(i *state.AlbumInfo) *string { return &i.Name }, false, func(u *immich.AlbumInfoUpdate, v string) { u.Name = immich.Some(v) }},
		{"description", func(i *state.AlbumInfo) *string { return &i.Description }, true, func(u *immich.AlbumInfoUpdate, v string) { u.Description = immich.Some(v) }},
	}
	for _, f := range afs {
		c0, c1 := *f.get(&cur[0]), *f.get(&cur[1])
		src := -1
		if known {
			ch0, ch1 := c0 != *f.get(base[0]), c1 != *f.get(base[1])
			switch {
			case !ch0 && !ch1:
				continue
			case ch0 && !ch1:
				src = 0
			case ch1 && !ch0:
				src = 1
			default:
				if c0 == c1 {
					continue
				}
				src = int(o.ConflictWinner)
			}
		} else {
			if c0 == c1 || !f.fill {
				continue
			}
			switch {
			case c0 == "":
				src = 1
			case c1 == "":
				src = 0
			default:
				continue
			}
		}
		dst := 1 - src
		v := *f.get(&cur[src])
		if f.name == "name" && v == "" {
			continue
		}
		if upds[dst] == nil {
			upds[dst] = &immich.AlbumInfoUpdate{}
		}
		f.set(upds[dst], v)
		*f.get(&newBase[dst]) = v
	}
	emit := upds[0] != nil || upds[1] != nil
	for s := 0; s < 2 && !emit; s++ {
		if base[s] == nil || *base[s] != newBase[s] {
			emit = true
		}
	}
	if !emit {
		return Action{}, false
	}
	act := Action{Kind: ActionAlbumInfo, AlbumUpdates: upds}
	for s := 0; s < 2; s++ {
		v := newBase[s]
		act.NewAlbumBase[s] = &v
	}
	return act, true
}
