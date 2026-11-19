package engine

import (
	"fmt"

	"immich-ferry/internal/engine/planner"
	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

// staleFields reports whether any field written by u differs between the
// fresh and the snapshot metadata.
func staleFields(u *immich.AssetUpdate, fresh, snap state.Meta) bool {
	if u.Description.Set && !planner.EqualDescription(fresh.Description, snap.Description) {
		return true
	}
	if u.DateTimeOriginal.Set && !planner.EqualDate(fresh.TakenAt, fresh.OffsetSec, snap.TakenAt, snap.OffsetSec) {
		return true
	}
	if (u.Latitude.Set || u.Longitude.Set) && !planner.EqualLocation(fresh.Lat, fresh.Lon, snap.Lat, snap.Lon) {
		return true
	}
	if u.Rating.Set && !planner.EqualRating(fresh.Rating, snap.Rating) {
		return true
	}
	if u.Visibility.Set && !planner.EqualVisibility(fresh.Visibility, snap.Visibility) {
		return true
	}
	return false
}

// metadataPhase applies meta and link_repair actions.
func (r *run) metadataPhase(acts []planner.Action) {
	r.progress(PhaseMetadata, 0, len(acts))
	for i, a := range acts {
		if r.ctx.Err() != nil {
			return
		}
		switch a.Kind {
		case planner.ActionLinkRepair:
			r.linkRepair(a)
		default:
			r.applyMeta(a)
		}
		r.tick()
		r.progress(PhaseMetadata, i+1, len(acts))
	}
}

func (r *run) applyMeta(a planner.Action) {
	key := string(a.Key)
	var written [2]*state.Meta
	var wroteID [2]string
	var known [2]bool
	var snapMeta [2]state.Meta
	allOK := true
	for s := A; s <= B; s++ {
		u := a.Updates[s]
		if u == nil {
			continue
		}
		id := a.TargetIDs[s]
		mem, isKnown := r.members[s][id]
		known[s], snapMeta[s] = isKnown, mem.Meta
		if isKnown && !mem.Owned {
			r.addSkip(SkipNotOwner, 1)
			allOK = false
			continue
		}
		cur, err := r.e.cl[s].GetAsset(r.ctx, id)
		if err != nil {
			allOK = false
			if r.canceled(err) {
				return
			}
			if immich.KindOf(err) == immich.KindNotFound {
				r.addSkip(SkipStale, 1)
				continue
			}
			r.itemError(key, "meta", s, err)
			continue
		}
		if isKnown && staleFields(u, normalizeMeta(cur), mem.Meta) {
			r.log.Info("asset changed since snapshot; skipping metadata write", "phase", "metadata", "key", key, "side", s.String(), "asset_id", id, "op", "meta")
			r.addSkip(SkipStale, 1)
			allOK = false
			continue
		}
		rb, err := r.e.cl[s].UpdateAsset(r.ctx, id, *u)
		r.mut()
		if err != nil {
			allOK = false
			if r.canceled(err) {
				return
			}
			r.itemError(key, "meta", s, err)
			continue
		}
		nm := normalizeMeta(rb)
		written[s], wroteID[s] = &nm, id
		r.count(func(sm *RunSummary) { dirInc(&sm.MetaUpdates, Side(1-s)) })
		r.log.Info("metadata updated", "phase", "metadata", "key", key, "side", s.String(), "asset_id", id, "op", "meta", "reason", a.Reason)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	it := r.ps.Item(key)
	for s := 0; s < 2; s++ {
		switch {
		case a.Updates[s] != nil:
			if written[s] != nil {
				// Only the fields we wrote are taken from the read-back; every other
				// field keeps the value we observed earlier, so a concurrent user edit
				// of an unwritten field is still seen as a change next cycle.
				var base state.Meta
				switch {
				case allOK && a.NewBase[s] != nil:
					base = cloneMetaT(*a.NewBase[s])
				case it.Meta[s] != nil && it.MetaID[s] == wroteID[s]:
					// Partial failure: the other side has not consumed our pending
					// change yet, so keep the old baseline for untouched fields.
					base = cloneMetaT(*it.Meta[s])
				case known[s]:
					base = cloneMetaT(snapMeta[s])
				default:
					base = *written[s]
				}
				overlayWritten(&base, a.Updates[s], *written[s])
				it.Meta[s], it.MetaID[s] = &base, wroteID[s]
			}
		case a.NewBase[s] != nil && allOK: // a failed write keeps the source baseline so the change is retried
			m := cloneMetaT(*a.NewBase[s])
			it.Meta[s], it.MetaID[s] = &m, a.NewBaseIDs[s]
		}
	}
	if a.ClearPending && allOK {
		it.Pending = nil
	}
}

// overlayWritten copies the fields written by u from the read-back rb into base.
func overlayWritten(base *state.Meta, u *immich.AssetUpdate, rb state.Meta) {
	if u.Description.Set {
		base.Description = rb.Description
	}
	if u.DateTimeOriginal.Set {
		c := cloneMetaT(rb)
		base.TakenAt, base.OffsetSec = c.TakenAt, c.OffsetSec
	}
	if u.Latitude.Set || u.Longitude.Set {
		c := cloneMetaT(rb)
		base.Lat, base.Lon = c.Lat, c.Lon
	}
	if u.Rating.Set {
		c := cloneMetaT(rb)
		base.Rating = c.Rating
	}
	if u.Visibility.Set {
		base.Visibility = rb.Visibility
	}
}

// linkRepair copies the source motion to the target and links the target still.
func (r *run) linkRepair(a planner.Action) {
	key := string(a.Key)
	from, to := Side(a.From), Side(a.To)
	if len(a.Refs) == 0 {
		return
	}
	srcMem, ok := r.members[from][a.Refs[0].ID]
	if !ok || srcMem.LivePhotoVideoID == "" {
		return
	}
	if mem, ok := r.members[to][a.TargetID]; ok && !mem.Owned {
		r.addSkip(SkipNotOwner, 1)
		return
	}
	cur, err := r.e.cl[to].GetAsset(r.ctx, a.TargetID)
	if err != nil {
		if !r.canceled(err) {
			r.itemError(key, "link_repair", to, err)
		}
		return
	}
	if cur.LivePhotoVideoID != nil && *cur.LivePhotoVideoID != "" {
		return
	}
	if cur.OwnerID != r.userIDs[to] {
		r.addSkip(SkipNotOwner, 1)
		return
	}
	err = r.linkLive(from, to, srcMem.LivePhotoVideoID, a.TargetID)
	switch {
	case err == nil:
		r.count(func(sm *RunSummary) { dirInc(&sm.Linked, from) })
		r.log.Info("live photo link repaired", "phase", "metadata", "key", key, "side", to.String(), "asset_id", a.TargetID, "op", "link_repair")
	case isMotionSkip(err):
		r.log.Warn("cannot repair live photo link", "phase", "metadata", "key", key, "side", to.String(), "op", "link_repair", "reason", err.Error())
	default:
		r.itemError(key, "link_repair", to, err)
	}
}

var _ = fmt.Sprint
