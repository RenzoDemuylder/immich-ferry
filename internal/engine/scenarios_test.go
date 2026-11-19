package engine

import (
	"bytes"
	"context"
	"testing"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
)

func TestInitialUnionIntoEmptyAlbum(t *testing.T) {
	e := newEnv(t)
	a1 := e.seedA("one")
	e.seedA("two", func(o *fakeimmich.SeedOpts) { o.Visibility = immich.VisibilityArchive })
	e.fa.Edit(a1, func(a *fakeimmich.FakeAsset) { a.IsFavorite = true })
	// B starts with an empty album, configured by id.
	albB := e.fb.CreateAlbum(e.ub, "Trip B")
	p := Pair{Name: "trip", A: AlbumRef{ID: e.albumA}, B: AlbumRef{ID: albB}}
	s, err := e.eng.RunPair(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Result != ResultOK || s.Copied.AtoB != 2 || s.Copied.BtoA != 0 || s.ItemErrorCount != 0 {
		t.Fatalf("summary: %+v", s)
	}
	if s.AlbumIDs[1] != albB {
		t.Fatalf("album ids %v, want B %s", s.AlbumIDs, albB)
	}
	for _, name := range []string{"one", "two"} {
		src, dst := e.onA(name), e.onB(name)
		if !bytes.Equal(src.Content, dst.Content) {
			t.Errorf("%s: content differs", name)
		}
		if !src.FileCreatedAt.Equal(dst.FileCreatedAt) || dst.OriginalFileName != src.OriginalFileName {
			t.Errorf("%s: filename/date not preserved: %+v vs %+v", name, src, dst)
		}
		if src.Visibility != dst.Visibility {
			t.Errorf("%s: visibility %s vs %s", name, src.Visibility, dst.Visibility)
		}
		if !contains(e.fb.AlbumAssetIDs(albB), dst.ID) {
			t.Errorf("%s not in B album", name)
		}
	}
	if e.onB("one").IsFavorite || e.onB("two").IsFavorite {
		t.Errorf("favorite carried (favorites are per user and not synced)")
	}
	for _, r := range e.fb.Requests() {
		for _, f := range r.Form {
			if f.Name == "isFavorite" {
				t.Errorf("upload sent isFavorite")
			}
		}
	}
	e.noTempFiles()
	if s.BytesDown == 0 || s.BytesUp == 0 || s.Mutations == 0 {
		t.Errorf("counters: %+v", s)
	}
}

func TestBidirectionalAdd(t *testing.T) {
	e := newEnv(t)
	e.seedA("a1")
	e.seedB("b1")
	s := e.run()
	if s.Copied.AtoB != 1 || s.Copied.BtoA != 1 || s.Result != ResultOK {
		t.Fatalf("%+v", s)
	}
	if !e.inAlbumB(e.onB("a1").ID) || !e.inAlbumA(e.onA("b1").ID) {
		t.Fatal("not synced")
	}
	if s.AlbumSizes != [2]int{1, 1} {
		t.Errorf("sizes %v", s.AlbumSizes)
	}
}

func TestRemovalUnlinksOtherSideOnly(t *testing.T) {
	e := newEnv(t)
	x := e.seedA("x")
	e.seedA("y")
	e.run()
	xb := e.onB("x").ID
	e.fa.RemoveFromAlbum(e.albumA, x)
	s := e.run()
	if s.Unlinked.AtoB != 1 || s.Unlinked.BtoA != 0 {
		t.Fatalf("%+v", s)
	}
	if e.inAlbumB(xb) {
		t.Error("still in B album")
	}
	if _, ok := e.fb.Asset(xb); !ok {
		t.Error("asset deleted on B")
	}
	if _, ok := e.fa.Asset(x); !ok {
		t.Error("asset deleted on A")
	}
	// and it stays removed
	s = e.run()
	if s.Mutations != 0 || s.Copied.AtoB+s.Copied.BtoA != 0 {
		t.Fatalf("not idempotent: %+v", s)
	}
}

func TestTrashOnBUnlinksOnA(t *testing.T) {
	e := newEnv(t)
	x := e.seedA("x")
	e.run()
	e.fb.Trash(e.onB("x").ID)
	s := e.run()
	if s.Unlinked.BtoA != 1 {
		t.Fatalf("%+v", s)
	}
	if e.inAlbumA(x) {
		t.Error("still in A album")
	}
	if s.Result != ResultOK {
		t.Errorf("result %s", s.Result)
	}
}

func TestOfflineExternalMemberNotUnlinked(t *testing.T) {
	e := newEnv(t)
	ext := e.seedB("ext", func(o *fakeimmich.SeedOpts) { o.LibraryID = "lib" })
	e.run()
	if !e.inAlbumA(e.onA("ext").ID) {
		t.Fatal("not copied to A")
	}
	e.fb.Trash(ext)
	e.fb.SetOffline(ext, true)
	s := e.run()
	if s.Unlinked.BtoA != 0 || s.Skipped[SkipOffline] != 1 {
		t.Fatalf("%+v", s)
	}
	if !e.inAlbumA(e.onA("ext").ID) {
		t.Error("unlinked on A")
	}
}

func TestHiddenMemberNotUnlinkedAndBlocked(t *testing.T) {
	e := newEnv(t)
	still := e.seedA("still")
	vid := e.seedA("vid", func(o *fakeimmich.SeedOpts) { o.Type = "VIDEO"; o.Filename = "vid.mp4" })
	e.fa.SetContentID(still, "c1")
	e.fa.SetContentID(vid, "c1")
	e.run()
	sb := e.onB("still")
	vb, _ := e.findByFile(e.fb, e.ub, "vid.mp4")
	e.fb.SetContentID(sb.ID, "c1")
	e.fb.SetContentID(vb.ID, "c1")
	e.fb.Extract(sb.ID) // auto-links: hides the video and removes it from album B
	if e.inAlbumB(vb.ID) {
		t.Fatal("fake did not remove motion from B album")
	}
	s := e.run()
	if s.Unlinked.BtoA != 0 || s.Skipped[SkipServerHidden] != 1 {
		t.Fatalf("%+v", s)
	}
	if !e.inAlbumA(vid) {
		t.Error("video wrongly unlinked from A")
	}
	ps := e.loadState()
	it := ps.Items[string(e.keyOf(e.fa.Asset(vid)))]
	if it == nil || it.Blocked == nil || it.Blocked.Reason != "server_hidden" || !it.InBoth {
		t.Fatalf("state: %+v", it)
	}
	s = e.run()
	if s.Skipped[SkipBlocked] != 1 || s.Unlinked.BtoA != 0 {
		t.Fatalf("second run %+v", s)
	}
	if !e.inAlbumA(vid) {
		t.Error("video wrongly unlinked from A")
	}
}

func (e *env) findByFile(srv *fakeimmich.Server, owner, file string) (fakeimmich.FakeAsset, bool) {
	for _, a := range srv.AssetsOf(owner) {
		if a.OriginalFileName == file {
			return a, true
		}
	}
	return fakeimmich.FakeAsset{}, false
}

func (e *env) keyOf(a fakeimmich.FakeAsset, _ bool) string { return a.Checksum }

func TestTrashedDuplicateRestoredButNotInBootstrap(t *testing.T) {
	for _, bootstrap := range []bool{false, true} {
		e := newEnv(t)
		if !bootstrap {
			e.seedA("warmup")
			e.run() // clears bootstrap
		}
		x := e.seedA("x")
		dup := e.seed(e.fb, e.ub, "", "x")
		e.fb.Trash(dup)
		s := e.run()
		if bootstrap {
			if s.Skipped[SkipTargetTrash] != 1 || s.Restored.AtoB != 0 {
				t.Fatalf("bootstrap: %+v", s)
			}
			if a, _ := e.fb.Asset(dup); !a.IsTrashed || e.inAlbumB(dup) {
				t.Error("must stay trashed and out of album")
			}
			continue
		}
		if s.Restored.AtoB != 1 || s.Copied.AtoB != 1 {
			t.Fatalf("%+v", s)
		}
		if a, _ := e.fb.Asset(dup); a.IsTrashed || !e.inAlbumB(dup) {
			t.Error("not restored/added")
		}
		if len(e.fb.AssetsOf(e.ub)) != 2 { // warmup copy + restored dup, no extra upload
			t.Errorf("unexpected uploads: %d", len(e.fb.AssetsOf(e.ub)))
		}
		_ = x
	}
}

func TestHiddenAndLockedDuplicateOnTarget(t *testing.T) {
	for _, vis := range []immich.Visibility{immich.VisibilityHidden, immich.VisibilityLocked} {
		e := newEnv(t)
		e.seedA("x")
		dup := e.seed(e.fb, e.ub, "", "x", func(o *fakeimmich.SeedOpts) { o.Visibility = vis })
		s := e.run()
		if s.Skipped[SkipTargetHidden] != 1 || s.Copied.AtoB != 0 {
			t.Fatalf("%s: %+v", vis, s)
		}
		if e.inAlbumB(dup) || len(e.fb.AlbumAssetIDs(e.albumB)) != 0 {
			t.Errorf("%s: added to album", vis)
		}
		s = e.run()
		if s.Skipped[SkipBlocked] != 1 {
			t.Errorf("%s second run: %+v", vis, s)
		}
	}
}

func TestPreexistingTargetCopyKeepsOwnDescription(t *testing.T) {
	e := newEnv(t)
	e.seedA("x", withExif("theirs"))
	dup := e.seed(e.fb, e.ub, "", "x", withExif("mine"))
	s := e.run()
	if s.Copied.AtoB != 1 || len(e.fb.AssetsOf(e.ub)) != 1 || !e.inAlbumB(dup) {
		t.Fatalf("%+v", s)
	}
	if e.fb.Requests() != nil && e.countReq(e.fb, "POST", "/assets") != 0 {
		t.Error("upload happened for existing duplicate")
	}
	e.run()
	e.run()
	if d := derefS(e.onB("x").Exif.Description); d != "mine" {
		t.Errorf("B description overwritten: %q", d)
	}
	if d := derefS(e.onA("x").Exif.Description); d != "theirs" {
		t.Errorf("A description overwritten: %q", d)
	}
}

func TestNoBaselineFillsEmptyDescription(t *testing.T) {
	e := newEnv(t)
	e.seedA("x", withExif(""))
	e.seedB("x", withExif("mine"))
	e.run()
	if d := derefS(e.onA("x").Exif.Description); d != "mine" {
		t.Errorf("A not filled: %q", d)
	}
}

func derefS(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestLivePhotoCopy(t *testing.T) {
	e := newEnv(t)
	mot := e.seedA("motion", func(o *fakeimmich.SeedOpts) {
		o.Type, o.Filename, o.Visibility = "VIDEO", "motion.mp4", immich.VisibilityHidden
	})
	e.seedA("still", func(o *fakeimmich.SeedOpts) { o.LivePhotoVideoID = mot })
	s := e.run()
	if s.Result != ResultOK || s.Copied.AtoB != 1 {
		t.Fatalf("%+v", s)
	}
	sb := e.onB("still")
	mb, ok := e.findByFile(e.fb, e.ub, "motion.mp4")
	if !ok || sb.LivePhotoVideoID != mb.ID || mb.Visibility != immich.VisibilityHidden || e.inAlbumB(mb.ID) {
		t.Fatalf("still=%+v motion=%+v", sb, mb)
	}
	if !e.inAlbumB(sb.ID) {
		t.Error("still not in album")
	}
	e.noTempFiles()
}

func TestLivePhotoTargetStillExistsUnlinked(t *testing.T) {
	e := newEnv(t)
	mot := e.seedA("motion", func(o *fakeimmich.SeedOpts) {
		o.Type, o.Filename, o.Visibility = "VIDEO", "motion.mp4", immich.VisibilityHidden
	})
	e.seedA("still", func(o *fakeimmich.SeedOpts) { o.LivePhotoVideoID = mot })
	dup := e.seed(e.fb, e.ub, "", "still")
	s := e.run()
	if s.Linked.AtoB != 1 || s.Copied.AtoB != 1 {
		t.Fatalf("%+v", s)
	}
	a, _ := e.fb.Asset(dup)
	mb, _ := e.findByFile(e.fb, e.ub, "motion.mp4")
	if a.LivePhotoVideoID != mb.ID || mb.Visibility != immich.VisibilityHidden || !e.inAlbumB(dup) {
		t.Fatalf("%+v %+v", a, mb)
	}
}

func TestLinkRepair(t *testing.T) {
	e := newEnv(t)
	mot := e.seedA("motion", func(o *fakeimmich.SeedOpts) {
		o.Type, o.Filename, o.Visibility = "VIDEO", "motion.mp4", immich.VisibilityHidden
	})
	e.seedA("still", func(o *fakeimmich.SeedOpts) { o.LivePhotoVideoID = mot })
	dup := e.seedB("still") // already in both albums, unlinked on B
	s := e.run()
	if s.Linked.AtoB != 1 {
		t.Fatalf("%+v", s)
	}
	a, _ := e.fb.Asset(dup)
	mb, ok := e.findByFile(e.fb, e.ub, "motion.mp4")
	if !ok || a.LivePhotoVideoID != mb.ID || mb.Visibility != immich.VisibilityHidden || e.inAlbumB(mb.ID) {
		t.Fatalf("%+v %+v", a, mb)
	}
	s = e.run()
	if s.Linked.AtoB != 0 || s.Mutations != 0 {
		t.Fatalf("repeat %+v", s)
	}
}

func TestLinkRepairNeverLinksVisibleMotion(t *testing.T) {
	e := newEnv(t)
	mot := e.seedA("motion", func(o *fakeimmich.SeedOpts) {
		o.Type, o.Filename, o.Visibility = "VIDEO", "motion.mp4", immich.VisibilityHidden
	})
	e.seedA("still", func(o *fakeimmich.SeedOpts) { o.LivePhotoVideoID = mot })
	dup := e.seedB("still")
	vis := e.seed(e.fb, e.ub, "", "motion", func(o *fakeimmich.SeedOpts) { o.Type, o.Filename = "VIDEO", "motion.mp4" })
	s := e.run()
	if s.Linked.AtoB != 0 {
		t.Fatalf("%+v", s)
	}
	if a, _ := e.fb.Asset(dup); a.LivePhotoVideoID != "" {
		t.Error("linked a visible motion")
	}
	if m, _ := e.fb.Asset(vis); m.Visibility != immich.VisibilityTimeline {
		t.Error("visible motion changed")
	}
}

func TestSharedAlbumOtherOwnersAsset(t *testing.T) {
	e := newEnv(t)
	other, _ := e.fa.AddUser("carol")
	e.fa.Share(e.albumA, other, "editor")
	o := e.seed(e.fa, other, e.albumA, "theirs")
	s := e.run()
	if s.Copied.AtoB != 1 || s.Result != ResultOK {
		t.Fatalf("%+v", s)
	}
	b := e.onB("theirs")
	if !e.inAlbumB(b.ID) || b.OwnerID != e.ub {
		t.Fatalf("%+v", b)
	}
	if !bytes.Equal(b.Content, e.content("theirs")) {
		t.Error("content")
	}
	_ = o
}

func TestFavoritesNeverSynced(t *testing.T) {
	e := newEnv(t)
	e.seedA("one")
	e.seedB("two")
	if s := e.run(); s.Copied.AtoB != 1 || s.Copied.BtoA != 1 {
		t.Fatalf("%+v", s)
	}
	e.extractAll()
	e.run() // settle pending metadata of the copies
	// Favorite on both sides (and a real change next to it), in both directions.
	e.fa.Edit(e.onA("one").ID, func(a *fakeimmich.FakeAsset) { a.IsFavorite = true })
	e.fb.Edit(e.onB("two").ID, func(a *fakeimmich.FakeAsset) { a.IsFavorite = true; a.Visibility = immich.VisibilityArchive })
	for i := 0; i < 2; i++ {
		e.run()
	}
	if e.onB("one").IsFavorite || e.onA("two").IsFavorite {
		t.Error("favorite propagated")
	}
	if !e.onA("one").IsFavorite || !e.onB("two").IsFavorite {
		t.Error("source favorite changed")
	}
	if e.onA("two").Visibility != immich.VisibilityArchive {
		t.Error("visibility change not synced next to favorite")
	}
	for _, srv := range []*fakeimmich.Server{e.fa, e.fb} {
		for _, r := range srv.Requests() {
			if r.Method == "PUT" && bytes.Contains(r.Body, []byte("isFavorite")) {
				t.Errorf("asset update sent isFavorite: %s", r.Body)
			}
		}
	}
}
