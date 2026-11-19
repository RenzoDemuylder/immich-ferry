package engine

import (
	"fmt"
	"strings"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

// Teardown of a discovered pair whose created album everyone has left.
//
// Discovery creates a counterpart album X' on side c (owned by the sync user) and shares
// it with DiscoveryShare[c]. Only the sync user could remove those members and it never
// does, so an X' without members means they left it. Then the sync user, in this order:
//  1. leaves the original album X on the other side (after that X is invisible to it, so
//     even a lost discovery state can never pair X again); if it owns X it cannot leave,
//     and X' is kept (empty of members) as a blocker so X never gets a new counterpart;
//  2. with DiscoveryTeardownTrash, trashes the assets it uploaded into X' that are in no
//     other album it can see;
//  3. deletes X' (Immich keeps the album's assets);
//  4. unpairs (pair state moved aside) and tombstones what is left.
//
// Every step is persisted in DiscoveredPair.Teardown before the next starts; an error
// keeps the progress and the step is retried next cycle. A pair in teardown is never
// synced again. Losing discovery.json loses the Created record and the progress: before
// the teardown the albums are re-paired by name and simply keep syncing (no members →
// no record → never torn down); a teardown interrupted after step 1 leaves X' behind, to
// be deleted by hand (like any lost tombstone, it may then get a new counterpart).

const teardownReason = "everyone the album was shared with has left it"

// shareOn reports whether albums created on side s are shared (teardown applies).
func (d *discovery) shareOn(s Side) bool { return len(d.e.o.DiscoveryShare[s]) > 0 }

// members returns the album's users other than the sync user of side s.
func (d *discovery) members(s Side, al immich.Album) []string {
	var out []string
	for _, u := range al.Users {
		if !strings.EqualFold(u.UserID, d.users[s]) {
			out = append(out, u.UserID)
		}
	}
	return out
}

// abandoned reports whether al (side s) is an album the sync user owns that nobody else
// is a member of, on a side whose created albums are shared. Only for a pair whose
// Created record proves the album was shared does this mean "everyone left".
func (d *discovery) abandoned(s Side, al immich.Album) bool {
	return d.shareOn(s) && roleOf(al, d.users[s]) == "owner" && len(d.members(s, al)) == 0
}

// leftover reports whether al (side s, not paired) must be left alone in dedicated-user
// mode: an album everyone left whose pair record may be lost.
func (d *discovery) leftover(s Side, al immich.Album) bool {
	return d.e.o.DiscoveryDedicatedUser && d.abandoned(s, al)
}

// backfill records the created album of a pair from before Created existed: the sync
// user owns exactly one of the two albums, on a shared side, and it has members.
func (d *discovery) backfill(p *state.DiscoveredPair, als [2]immich.Album) {
	if p.Created != nil {
		return
	}
	for _, c := range []Side{A, B} {
		o := 1 - c
		m := d.members(c, als[c])
		if d.shareOn(c) && roleOf(als[c], d.users[c]) == "owner" && roleOf(als[o], d.users[o]) != "owner" && len(m) > 0 {
			p.Created = &state.CreatedAlbum{Side: uint8(c), SharedWith: m}
			d.e.log.Info("recorded album created by discovery", "op", "discovery", "pair", p.Name,
				"side", c.String(), "album_id", als[c].ID, "shared_with", m)
			return
		}
	}
}

// shouldTeardown checks whether the created album of p was left by everyone, confirming
// a listing with a fresh GET. It reports whether the pair is (now) in teardown, and
// whether the check failed (pair kept, not synced this cycle).
func (d *discovery) shouldTeardown(p *state.DiscoveredPair, als [2]immich.Album) (yes, failed bool) {
	if p.Created == nil || p.Created.Side > 1 || len(p.Created.SharedWith) == 0 {
		return false, false
	}
	c := Side(p.Created.Side)
	if !d.abandoned(c, als[c]) {
		return false, false
	}
	al, err := d.e.cl[c].GetAlbum(d.ctx, p.AlbumIDs[c])
	if err != nil {
		if d.ctx.Err() == nil {
			d.note(&d.rep.Errors, AlbumNote{Pair: p.Name, Side: c.String(), AlbumID: p.AlbumIDs[c],
				Reason: "confirm that everyone left the album: " + err.Error()})
		}
		return false, true
	}
	return d.abandoned(c, al), false
}

// teardown runs (or resumes) the teardown of p and reports whether the pair must be kept
// in the discovery state (dry run, or a step failed and is retried next cycle).
func (d *discovery) teardown(p *state.DiscoveredPair) (keep bool) {
	c := Side(p.Created.Side)
	o := 1 - c
	if !d.mutating {
		d.note(&d.rep.WouldTeardown, AlbumNote{Pair: p.Name, Side: c.String(), AlbumID: p.AlbumIDs[c], Reason: teardownReason})
		return true
	}
	if p.Teardown == nil {
		p.Teardown = &state.TeardownProgress{Since: d.e.o.Now()}
		d.e.log.Warn("tearing down album pair", "op", "teardown", "pair", p.Name, "side", c.String(),
			"album_id", p.AlbumIDs[c], "reason", teardownReason)
		if !d.save(p) {
			return true
		}
	}
	tp := p.Teardown
	fail := func(step string, err error) bool {
		if d.ctx.Err() == nil {
			d.note(&d.rep.Errors, AlbumNote{Pair: p.Name, Reason: "teardown: " + step + ": " + err.Error()})
			d.e.log.Error("teardown step failed; retried next cycle", "op", "teardown", "pair", p.Name,
				"phase", step, "kind", immich.KindOf(err).String(), "err", err)
		}
		return true
	}

	if !tp.Left {
		al, gone, err := d.lookup(o, p.AlbumIDs[o])
		switch {
		case err != nil:
			return fail("read album on side "+o.String(), err)
		case gone:
		case roleOf(al, d.users[o]) == "owner":
			tp.KeptAlbum = true
			d.e.log.Warn("sync user owns the original album and cannot leave it; keeping the emptied counterpart so it is never re-created",
				"op", "teardown", "pair", p.Name, "side", o.String(), "album_id", al.ID)
		default:
			if err := d.e.cl[o].LeaveAlbum(d.ctx, al.ID); err != nil && !d.confirmGone(o, al.ID) {
				return fail("leave album on side "+o.String(), err)
			}
			d.e.log.Info("left album", "op", "leave_album", "pair", p.Name, "side", o.String(), "album_id", al.ID)
		}
		tp.Left = true
		if !d.save(p) {
			return true
		}
	}

	if !tp.AssetsDone {
		if d.e.o.DiscoveryTeardownTrash {
			n, err := d.trashOurs(p, c)
			if err != nil {
				return fail("trash assets on side "+c.String(), err)
			}
			if n > 0 {
				d.note(&d.rep.Trashed, AlbumNote{Pair: p.Name, Side: c.String(), AlbumID: p.AlbumIDs[c], Assets: n})
			}
		}
		tp.AssetsDone = true
		if !d.save(p) {
			return true
		}
	}

	if !tp.AlbumDeleted && !tp.KeptAlbum {
		id := p.AlbumIDs[c]
		if err := d.e.cl[c].DeleteAlbum(d.ctx, id); err != nil && !d.confirmGone(c, id) {
			return fail("delete album on side "+c.String(), err)
		}
		d.e.log.Info("deleted album", "op", "delete_album", "pair", p.Name, "side", c.String(), "album_id", id)
		tp.AlbumDeleted = true
		if !d.save(p) {
			return true
		}
	}

	d.unpair(*p, teardownReason)
	d.tombstone(o, p.AlbumIDs[o], teardownReason)
	if tp.KeptAlbum {
		d.tombstone(c, p.AlbumIDs[c], teardownReason)
	}
	d.note(&d.rep.TornDown, AlbumNote{Pair: p.Name, Side: c.String(), AlbumID: p.AlbumIDs[c], Reason: teardownReason})
	return false
}

// confirmGone reports whether album id on side s is definitely not accessible anymore
// (e.g. a retried leave/delete whose first attempt succeeded).
func (d *discovery) confirmGone(s Side, id string) bool {
	_, err := d.e.cl[s].GetAlbum(d.ctx, id)
	return err != nil && immich.KindOf(err) == immich.KindNotFound && d.ctx.Err() == nil
}

// save persists the discovery state with p updated, before the next teardown step.
func (d *discovery) save(p *state.DiscoveredPair) bool {
	if err := d.e.st.SaveDiscovery(d.st); err != nil {
		d.note(&d.rep.Errors, AlbumNote{Pair: p.Name, Reason: "save discovery state: " + err.Error()})
		return false
	}
	return true
}

func (d *discovery) tombstone(s Side, id, reason string) {
	if d.tomb[s][id] {
		return
	}
	d.tomb[s][id] = true
	d.torn[s][id] = reason == teardownReason
	d.st.Tombstones = append(d.st.Tombstones, state.Tombstone{Side: uint8(s), AlbumID: id, Reason: reason, Since: d.e.o.Now()})
}

// trashOurs trashes the assets in the created album (side c) that this tool uploaded
// (recorded in the pair state) and that are in no other album the sync user can see.
// Without a pair state nothing is provably ours, so nothing is trashed.
func (d *discovery) trashOurs(p *state.DiscoveredPair, c Side) (int, error) {
	ps, _, err := d.e.st.Peek(p.Name)
	if err != nil {
		return 0, fmt.Errorf("read pair state: %w", err)
	}
	if ps.AlbumIDs != [2]string{} && ps.AlbumIDs != p.AlbumIDs {
		return 0, nil
	}
	ours := map[string]bool{}
	for _, it := range ps.Items {
		if it != nil && it.Ours[c] != "" {
			ours[it.Ours[c]] = true
		}
	}
	if len(ours) == 0 {
		return 0, nil
	}
	albumID := p.AlbumIDs[c]
	assets, err := d.e.cl[c].ListAlbumAssets(d.ctx, albumID)
	if err != nil {
		return 0, fmt.Errorf("list album assets: %w", err)
	}
	var ids []string
	for _, a := range assets {
		if !ours[a.ID] || !strings.EqualFold(a.OwnerID, d.users[c]) {
			continue
		}
		als, err := d.e.cl[c].AlbumsWithAsset(d.ctx, a.ID)
		if err != nil {
			if d.ctx.Err() != nil {
				return 0, d.ctx.Err()
			}
			d.e.log.Warn("cannot list the albums of an asset; keeping it", "op", "teardown", "pair", p.Name,
				"side", c.String(), "asset_id", a.ID, "kind", immich.KindOf(err).String(), "err", err)
			continue
		}
		elsewhere := false
		for _, al := range als {
			if al.ID != albumID {
				elsewhere = true
			}
		}
		if !elsewhere {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := d.e.cl[c].TrashAssets(d.ctx, ids); err != nil {
		return 0, err
	}
	d.e.log.Info("trashed assets", "op", "trash", "pair", p.Name, "side", c.String(), "album_id", albumID, "assets", len(ids))
	return len(ids), nil
}
