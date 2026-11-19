package engine

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"sort"
	"time"

	"immich-ferry/internal/engine/planner"
	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

func visible(v immich.Visibility) bool {
	return v == immich.VisibilityTimeline || v == immich.VisibilityArchive
}

// normalizeMeta converts an asset into the normalized mergeable metadata. Favorites are
// per user in Immich and deliberately not synced.
func normalizeMeta(a immich.Asset) state.Meta {
	m := state.Meta{Visibility: string(a.Visibility)}
	e := a.Exif
	if e == nil {
		return m
	}
	if e.Description != nil {
		m.Description = *e.Description
	}
	if e.DateTimeOriginal != nil {
		t := e.DateTimeOriginal.UTC()
		m.TakenAt = &t
		if e.TimeZone != nil {
			if off, ok := planner.OffsetSeconds(*e.TimeZone, t); ok {
				m.OffsetSec = &off
			}
		}
	}
	if e.Latitude != nil && e.Longitude != nil {
		la, lo := *e.Latitude, *e.Longitude
		m.Lat, m.Lon = &la, &lo
	}
	if e.Rating != nil {
		v := *e.Rating
		m.Rating = &v
	}
	return m
}

func sha1b64(sum []byte) string { return base64.StdEncoding.EncodeToString(sum) }

// snapshotSide lists one album and groups its assets by key.
func (r *run) snapshotSide(s Side) error {
	r.progress(PhaseSnapshot, int(s), 2)
	snap := planner.Snapshot{AlbumID: r.albumIDs[s], Consistent: true, Groups: map[planner.Key][]planner.Member{}}
	r.assets[s] = map[string]immich.Asset{}
	r.members[s] = map[string]planner.Member{}
	if r.albumIDs[s] == "" {
		r.snaps[s] = snap
		return nil
	}
	cl := r.e.cl[s]
	before, err := cl.GetAlbum(r.ctx, r.albumIDs[s])
	if err != nil {
		return fmt.Errorf("snapshot side %s: get album: %w", s, err)
	}
	items, err := cl.ListAlbumAssets(r.ctx, r.albumIDs[s])
	if err != nil {
		return fmt.Errorf("snapshot side %s: list assets: %w", s, err)
	}
	after, err := cl.GetAlbum(r.ctx, r.albumIDs[s])
	if err != nil {
		return fmt.Errorf("snapshot side %s: get album: %w", s, err)
	}
	snap.Consistent = before.AssetCount == after.AssetCount && len(items) == after.AssetCount
	snap.AlbumName, snap.AlbumDescription = after.Name, after.Description
	r.sum.SnapshotConsistent[s] = snap.Consistent
	if !snap.Consistent {
		r.log.Warn("album snapshot inconsistent", "phase", "snapshot", "side", s.String(),
			"count_before", before.AssetCount, "count_after", after.AssetCount, "listed", len(items))
	}
	r.progress(PhaseSnapshot, int(s), 2)
	listed := 0
	for _, a := range items {
		if a.IsTrashed || !visible(a.Visibility) {
			continue
		}
		listed++
		key := a.Checksum
		external := a.LibraryID != nil
		if external {
			k, err := r.hashExternal(s, a)
			if err != nil {
				if r.canceled(err) {
					return err
				}
				r.itemError(a.ID, "hash", s, err)
				// The asset's key is unknown, so it is missing from this side's
				// groups: copies into / drops for this side are unsafe this cycle.
				r.incomplete[s] = true
				continue
			}
			key = k
		}
		owned := a.OwnerID == r.userIDs[s]
		m := planner.Member{
			Ref:              state.AssetRef{ID: a.ID, Checksum: a.Checksum},
			Owned:            owned,
			External:         external,
			Meta:             normalizeMeta(a),
			LivePhotoVideoID: deref(a.LivePhotoVideoID),
			Extracted:        a.Exif != nil && a.Exif.DateTimeOriginal != nil,
			Visibility:       a.Visibility,
			FileCreatedAt:    a.FileCreatedAt,
		}
		snap.Groups[planner.Key(key)] = append(snap.Groups[planner.Key(key)], m)
		r.assets[s][a.ID] = a
		r.members[s][a.ID] = m
	}
	for k, g := range snap.Groups {
		sort.SliceStable(g, func(i, j int) bool {
			if g[i].Owned != g[j].Owned {
				return g[i].Owned
			}
			return g[i].Ref.ID < g[j].Ref.ID
		})
		snap.Groups[k] = g
	}
	r.sum.AlbumSizes[s] = listed
	r.snaps[s] = snap
	r.progress(PhaseSnapshot, int(s)+1, 2)
	return nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// hashExternal returns the content hash of an external-library asset, using
// the state cache (assetID + fileModifiedAt) when possible.
func (r *run) hashExternal(s Side, a immich.Asset) (string, error) {
	r.mu.Lock()
	h, ok := r.ps.Hashes[a.ID]
	r.mu.Unlock()
	if ok && h.Key != "" && h.FileModifiedAt.Equal(a.FileModifiedAt) {
		return h.Key, nil
	}
	hs := sha1.New()
	n, err := r.e.cl[s].DownloadOriginal(r.ctx, a.ID, hs, func(n int64) { r.bytesDown.Add(n); r.byteTick() })
	if err != nil {
		return "", fmt.Errorf("hash external asset %s: %w", a.ID, err)
	}
	key := sha1b64(hs.Sum(nil))
	r.log.Debug("hashed external asset", "phase", "snapshot", "side", s.String(), "asset_id", a.ID, "bytes", n)
	r.mu.Lock()
	r.ps.Hashes[a.ID] = state.HashEntry{FileModifiedAt: a.FileModifiedAt, Key: key}
	r.mu.Unlock()
	return key, nil
}

// gcHashes drops cache entries for assets not seen in either snapshot.
func (r *run) gcHashes() {
	seen := map[string]bool{}
	for s := 0; s < 2; s++ {
		for id := range r.assets[s] {
			seen[id] = true
		}
	}
	for id := range r.ps.Hashes {
		if !seen[id] {
			delete(r.ps.Hashes, id)
		}
	}
}

var _ = context.Background
var _ = time.Now
