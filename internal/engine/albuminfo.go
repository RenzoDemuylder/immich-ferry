package engine

import (
	"immich-ferry/internal/engine/planner"
	"immich-ferry/internal/state"
)

// albumInfoPhase applies the album name/description merge.
func (r *run) albumInfoPhase(acts []planner.Action) {
	r.progress(PhaseAlbumInfo, 0, len(acts))
	for i, a := range acts {
		if r.ctx.Err() != nil {
			return
		}
		var written [2]*state.AlbumInfo
		for s := A; s <= B; s++ {
			u := a.AlbumUpdates[s]
			if u == nil {
				continue
			}
			cl := r.e.cl[s]
			cur, err := cl.GetAlbum(r.ctx, r.albumIDs[s])
			if err != nil {
				if r.canceled(err) {
					return
				}
				r.itemError("", "albuminfo", s, err)
				continue
			}
			snap := r.snaps[s]
			if (u.Name.Set && cur.Name != snap.AlbumName) || (u.Description.Set && cur.Description != snap.AlbumDescription) {
				r.log.Info("album changed since snapshot; skipping album info write", "phase", "albuminfo", "side", s.String(), "op", "albuminfo")
				r.addSkip(SkipStale, 1)
				continue
			}
			rb, err := cl.UpdateAlbumInfo(r.ctx, r.albumIDs[s], *u)
			r.mut()
			if err != nil {
				if r.canceled(err) {
					return
				}
				r.itemError("", "albuminfo", s, err)
				continue
			}
			written[s] = &state.AlbumInfo{Name: rb.Name, Description: rb.Description}
			r.count(func(sm *RunSummary) { dirInc(&sm.AlbumInfoUpdated, Side(1-s)) })
			r.log.Info("album info updated", "phase", "albuminfo", "side", s.String(), "op", "albuminfo")
		}
		allOK := true
		for s := 0; s < 2; s++ {
			if a.AlbumUpdates[s] != nil && written[s] == nil {
				allOK = false
			}
		}
		r.mu.Lock()
		for s := 0; s < 2; s++ {
			switch {
			case a.AlbumUpdates[s] != nil:
				if written[s] == nil {
					continue
				}
				// Only the fields we wrote come from the read-back.
				var base state.AlbumInfo
				switch {
				case allOK && a.NewAlbumBase[s] != nil:
					base = *a.NewAlbumBase[s]
				case r.ps.AlbumInfo[s] != nil:
					base = *r.ps.AlbumInfo[s]
				default:
					base = state.AlbumInfo{Name: r.snaps[s].AlbumName, Description: r.snaps[s].AlbumDescription}
				}
				if a.AlbumUpdates[s].Name.Set {
					base.Name = written[s].Name
				}
				if a.AlbumUpdates[s].Description.Set {
					base.Description = written[s].Description
				}
				r.ps.AlbumInfo[s] = &base
			case a.NewAlbumBase[s] != nil && allOK:
				v := *a.NewAlbumBase[s]
				r.ps.AlbumInfo[s] = &v
			}
		}
		r.mu.Unlock()
		r.progress(PhaseAlbumInfo, i+1, len(acts))
	}
}
