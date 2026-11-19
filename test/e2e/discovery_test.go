//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"immich-ferry/internal/engine"
	"immich-ferry/test/e2e/testutil"
)

// TestSyncDiscoveryMarker: albums marked with an emoji in the description (on either side) are
// paired by name or get a counterpart created; unmarked albums are ignored; a deleted counterpart
// is never recreated.
func TestSyncDiscoveryMarker(t *testing.T) {
	s := newSyncer(t, func(o *engine.Options) {
		o.Discovery = engine.DiscoveryMarker
		o.DiscoveryMarker = "🔁"
	})
	pa := photo(t, s.ua, "disc-a.jpg", t0)
	pb := photo(t, s.ub, "disc-b.jpg", t0.Add(time.Hour))
	pu := photo(t, s.ua, "disc-unmarked.jpg", t0.Add(2*time.Hour))
	tripA, err := s.ua.API.CreateAlbum(s.ctx, s.name+"-trip", "summer 🔁", pa.ID)
	if err != nil {
		t.Fatal(err)
	}
	tripB, err := s.ub.API.CreateAlbum(s.ctx, s.name+"-trip", "") // unmarked counterpart, paired by name
	if err != nil {
		t.Fatal(err)
	}
	onlyB, err := s.ub.API.CreateAlbum(s.ctx, s.name+"-onlyb", "🔁", pb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ua.API.CreateAlbum(s.ctx, s.name+"-unmarked", "", pu.ID); err != nil {
		t.Fatal(err)
	}

	pairs, rep, err := s.eng.Pairs(s.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("discovery: %+v", rep)
	if len(pairs) != 2 || len(rep.Created) != 1 || rep.Created[0].Side != "a" || rep.Problems() {
		t.Fatalf("pairs %+v report %+v", pairs, rep)
	}
	var onlyBA string
	for _, p := range pairs {
		switch {
		case p.A.ID == tripA && p.B.ID != tripB:
			t.Fatalf("trip paired with %s, want %s", p.B.ID, tripB)
		case p.B.ID == onlyB:
			onlyBA = p.A.ID
		}
		if sum, err := s.eng.RunPair(s.ctx, p); err != nil || sum.ItemErrorCount > 0 {
			t.Fatalf("RunPair %s: %v %+v", p.Name, err, sum)
		}
	}
	al, err := s.ua.API.GetAlbum(s.ctx, onlyBA)
	if err != nil || al.Name != s.name+"-onlyb" || al.Description != "🔁" {
		t.Fatalf("created album %+v err %v", al, err)
	}
	if _, ok := s.albumSet(s.ub.API, tripB)[pa.Sum]; !ok {
		t.Error("trip photo not copied to B")
	}
	if _, ok := s.albumSet(s.ua.API, onlyBA)[pb.Sum]; !ok {
		t.Error("onlyb photo not copied to A")
	}
	for _, al := range mustList(t, s, s.ub.API) {
		if al.Name == s.name+"-unmarked" {
			t.Error("unmarked album synced")
		}
	}

	// Deleting the counterpart on A stops syncing that pair and never recreates it.
	if err := s.ua.API.DeleteAlbum(s.ctx, onlyBA); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		pairs, rep, err = s.eng.Pairs(s.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(pairs) != 1 || len(rep.Created) != 0 {
			t.Fatalf("pass %d after delete: pairs %+v report %+v", i, pairs, rep)
		}
	}
	n := 0
	for _, al := range mustList(t, s, s.ua.API) {
		if al.Name == s.name+"-onlyb" {
			n++
		}
	}
	if n != 0 {
		t.Fatalf("deleted album recreated on A (%d)", n)
	}
	if got := s.mustAlbumAssets(s.ub.API, onlyB); len(got) != 1 {
		t.Errorf("B album changed after unpairing: %v", got)
	}
}

func mustList(t *testing.T, s *syncer, api *testutil.API) []testutil.Album {
	t.Helper()
	l, err := api.ListAlbums(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestSyncDiscoveryShare: albums created by discovery are owned by the sync user and shared,
// in the create request itself, with the configured users (by email or id); a friend on the
// target side then sees the album and the synced photo. Pre-existing albums are not shared.
func TestSyncDiscoveryShare(t *testing.T) {
	instA, instB := e2eInstances(t)
	ctx := context.Background()
	friendA, err := testutil.NewUser(ctx, instA, "friend-a")
	if err != nil {
		t.Fatal(err)
	}
	friendB, err := testutil.NewUser(ctx, instB, "friend-b")
	if err != nil {
		t.Fatal(err)
	}
	s := newSyncer(t, func(o *engine.Options) {
		o.Discovery = engine.DiscoveryMarker
		o.DiscoveryMarker = "🔁"
		o.DiscoveryShare = [2][]string{{friendA.ID}, {strings.ToUpper(friendB.Email)}}
	})
	pa := photo(t, s.ua, "share-a.jpg", t0)
	pb := photo(t, s.ub, "share-b.jpg", t0.Add(time.Hour))
	fromA, err := s.ua.API.CreateAlbum(s.ctx, s.name+"-froma", "🔁", pa.ID)
	if err != nil {
		t.Fatal(err)
	}
	fromB, err := s.ub.API.CreateAlbum(s.ctx, s.name+"-fromb", "🔁", pb.ID)
	if err != nil {
		t.Fatal(err)
	}
	pairs, rep, err := s.eng.Pairs(s.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 || len(rep.Created) != 2 || rep.Problems() {
		t.Fatalf("pairs %+v report %+v", pairs, rep)
	}
	created := map[string]string{} // side -> created album id
	for _, p := range pairs {
		if sum, err := s.eng.RunPair(s.ctx, p); err != nil || sum.ItemErrorCount > 0 {
			t.Fatalf("RunPair %s: %v %+v", p.Name, err, sum)
		}
		if p.A.ID == fromA {
			created["b"] = p.B.ID
		}
		if p.B.ID == fromB {
			created["a"] = p.A.ID
		}
	}
	check := func(side string, owner, friend *testutil.User, albumID, sum string) {
		t.Helper()
		al, err := friend.API.GetAlbum(s.ctx, albumID)
		if err != nil {
			t.Fatalf("side %s: friend cannot read created album: %v", side, err)
		}
		roles := map[string]string{}
		for _, u := range al.Users {
			roles[u.User.ID] = u.Role
		}
		if len(roles) != 2 || roles[owner.ID] != "owner" || roles[friend.ID] != "editor" {
			t.Errorf("side %s: created album users %v", side, roles)
		}
		if _, ok := s.albumSet(friend.API, albumID)[sum]; !ok {
			t.Errorf("side %s: friend does not see the synced photo", side)
		}
	}
	check("b", s.ub, friendB, created["b"], pa.Sum)
	check("a", s.ua, friendA, created["a"], pb.Sum)
	for _, x := range []struct {
		api *testutil.API
		id  string
	}{{s.ua.API, fromA}, {s.ub.API, fromB}} {
		if al, err := x.api.GetAlbum(s.ctx, x.id); err != nil || len(al.Users) != 1 {
			t.Errorf("pre-existing album %s changed: %+v %v", x.id, al, err)
		}
	}
}

// TestSyncDiscoveryTeardown: carol on A shares her album with the sync user; discovery
// creates its counterpart on B shared with a friend. When the friend leaves it, the sync
// user leaves carol's album, trashes what it uploaded into the counterpart, deletes the
// counterpart and stops syncing. Carol's album and photos are untouched.
func TestSyncDiscoveryTeardown(t *testing.T) {
	instA, instB := e2eInstances(t)
	ctx := context.Background()
	carol, err := testutil.NewUser(ctx, instA, "carol")
	if err != nil {
		t.Fatal(err)
	}
	friendB, err := testutil.NewUser(ctx, instB, "friend-b")
	if err != nil {
		t.Fatal(err)
	}
	s := newSyncer(t, func(o *engine.Options) {
		o.Discovery = engine.DiscoveryMarker
		o.DiscoveryMarker = "🔁"
		o.DiscoveryShare = [2][]string{nil, {friendB.Email}}
		o.DiscoveryTeardownTrash = true
	})
	pc := photo(t, carol, "teardown-a.jpg", t0)
	hol, err := carol.API.CreateAlbum(ctx, s.name+"-holiday", "🔁", pc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := carol.API.ShareAlbum(ctx, hol, s.ua.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	pairs, rep, err := s.eng.Pairs(ctx, nil)
	if err != nil || len(pairs) != 1 || len(rep.Created) != 1 || rep.Problems() {
		t.Fatalf("pairs %+v report %+v err %v", pairs, rep, err)
	}
	created := pairs[0].B.ID
	if sum, err := s.eng.RunPair(ctx, pairs[0]); err != nil || sum.Copied.AtoB != 1 {
		t.Fatalf("RunPair: %v %+v", err, sum)
	}
	copied, ok := s.albumSet(s.ub.API, created)[pc.Sum]
	if !ok {
		t.Fatal("photo not synced to B")
	}

	if err := friendB.API.Do(ctx, http.MethodDelete, "/albums/"+created+"/user/me", nil, nil); err != nil {
		t.Fatalf("friend leaves: %v", err)
	}
	pairs, rep, err = s.eng.Pairs(ctx, nil)
	if err != nil || len(pairs) != 0 || len(rep.TornDown) != 1 || len(rep.Trashed) != 1 || rep.Trashed[0].Assets != 1 || len(rep.Errors) != 0 {
		t.Fatalf("teardown: pairs %+v report %+v err %v", pairs, rep, err)
	}
	if _, err := s.ub.API.GetAlbum(ctx, created); testutil.StatusOf(err) != 400 {
		t.Errorf("created album still readable: %v", err)
	}
	if a, err := s.ub.API.GetAssetTyped(ctx, copied.ID); err != nil || !a.IsTrashed {
		t.Errorf("copied photo not trashed: %+v %v", a, err)
	}
	al, err := carol.API.GetAlbum(ctx, hol)
	if err != nil || len(al.Users) != 1 {
		t.Fatalf("sync user still a member of carol's album: %+v %v", al, err)
	}
	if _, ok := s.albumSet(carol.API, hol)[pc.Sum]; !ok {
		t.Error("carol's photo gone from her album")
	}
	pairs, rep, err = s.eng.Pairs(ctx, nil)
	if err != nil || len(pairs)+len(rep.Created)+len(rep.TornDown) != 0 {
		t.Fatalf("after teardown: pairs %+v report %+v err %v", pairs, rep, err)
	}
}
