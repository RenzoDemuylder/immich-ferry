package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"immich-ferry/internal/engine/planner"
	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
	"immich-ferry/internal/state"
)

// stateItem returns the state item of key from the persisted state.
func (e *env) stateItem(key string) *state.Item {
	e.t.Helper()
	it := e.loadState().Items[key]
	if it == nil {
		e.t.Fatalf("no state item for key %s", key)
	}
	return it
}

// directRun builds a minimal run on e's engine for calling phase methods with
// hand-made actions (defensive paths the planner never produces).
func (e *env) directRun(members [2]map[string]planner.Member) *run {
	return &run{e: e.eng, o: e.eng.o, ctx: context.Background(), p: e.pair(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), mutating: true,
		ps: state.NewPairState("trip"), sum: RunSummary{Pair: "trip", Skipped: map[SkipReason]int{}},
		userIDs: [2]string{e.ua, e.ub}, members: members, budget: newByteBudget(e.opts.TmpBudgetBytes),
		motionLks: map[string]*sync.Mutex{}, lastSave: e.clk.Now()}
}

// Crash after our upload reached B (write-ahead Intent with UploadStarted
// persisted) but before the album add result / final save was recorded: the
// next run recovers the Intent into Pending, records the asset as ours and
// applies source-wins metadata once the target is extracted.
func TestRecoverIntentUploadStartedBecomesPending(t *testing.T) {
	e := newEnv(t)
	a := e.seedA("x", withExif("A-desc"))
	key := e.checksumA(a)
	path := e.dir + "/pairs/trip.json"
	var saved []byte
	var once sync.Once
	hb := &hookImmich{Immich: e.clB}
	hb.onUpload = func(ctx context.Context, r immich.UploadRequest, onBytes func(int64)) (immich.UploadResult, error) {
		once.Do(func() {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Error(err)
			}
			saved = b
		})
		return e.clB.Upload(ctx, r, onBytes)
	}
	e.eng = e.engineWith(e.clA, hb)
	if s := e.run(); s.Copied.AtoB != 1 {
		t.Fatalf("copy: %+v", s)
	}
	bid := e.onB("x").ID
	if !e.inAlbumB(bid) {
		t.Fatal("uploaded asset not in album B")
	}
	// Simulate SIGKILL right after the write-ahead save: only the Intent survives.
	if err := os.WriteFile(path, saved, 0o644); err != nil {
		t.Fatal(err)
	}
	pre := e.stateItem(key)
	if pre.Intent == nil || !pre.Intent.UploadStarted || pre.Intent.To != uint8(B) || pre.Pending != nil || pre.Ours[B] != "" {
		t.Fatalf("restored state is not the write-ahead snapshot: %+v intent=%+v", pre, pre.Intent)
	}
	started := pre.Intent.Started

	e.eng = e.engineWith(e.clA, e.clB)
	e.clk.Advance(time.Minute)
	s := e.run() // target not extracted yet: Pending must hold the key
	if s.Pending != 1 || s.MetaUpdates.AtoB != 0 || s.ItemErrorCount != 0 {
		t.Fatalf("recovery run: %+v", s)
	}
	it := e.stateItem(key)
	if it.Intent != nil {
		t.Errorf("intent not cleared: %+v", it.Intent)
	}
	if it.Pending == nil || it.Pending.To != uint8(B) || it.Pending.TargetID != bid || !it.Pending.Since.Equal(started) {
		t.Errorf("pending = %+v, want To=B TargetID=%s Since=%v", it.Pending, bid, started)
	}
	if it.Ours[B] != bid || it.Ours[A] != "" {
		t.Errorf("ours = %v, want [\"\" %s]", it.Ours, bid)
	}
	if !it.InBoth || it.MetaID[A] != a || it.MetaID[B] != bid {
		t.Errorf("inBoth=%v metaID=%v", it.InBoth, it.MetaID)
	}
	for s := A; s <= B; s++ {
		if it.Meta[s] == nil || it.Meta[s].Description != "A-desc" {
			t.Errorf("baseline %v = %+v, want source meta", s, it.Meta[s])
		}
	}

	// Extraction gives the target its own differing description: source wins.
	e.fb.Edit(bid, func(x *fakeimmich.FakeAsset) {
		if x.Exif == nil {
			x.Exif = &immich.Exif{}
		}
		d := "B-extracted"
		x.Exif.Description = &d
	})
	e.fb.Extract(bid)
	s = e.run()
	if s.MetaUpdates.AtoB != 1 || s.MetaUpdates.BtoA != 0 {
		t.Fatalf("source-wins run: %+v", s)
	}
	if d := derefS(e.onB("x").Exif.Description); d != "A-desc" {
		t.Errorf("B description = %q, want source value A-desc", d)
	}
	if d := derefS(e.onA("x").Exif.Description); d != "A-desc" {
		t.Errorf("A description = %q, want unchanged A-desc", d)
	}
	// Pending is cleared by the first cycle without an EXIF write (DESIGN Appendix E).
	if s = e.run(); s.Pending != 0 || s.Mutations != 0 {
		t.Errorf("not converged: %+v", s)
	}
	if it := e.stateItem(key); it.Pending != nil || it.Ours[B] != bid {
		t.Errorf("after source-wins: pending=%+v ours=%v", it.Pending, it.Ours)
	}
	if d := derefS(e.onB("x").Exif.Description); d != "A-desc" {
		t.Errorf("B description = %q after convergence", d)
	}
}

// A leftover Intent for a key that already has Pending is only cleared; the
// existing Pending, Ours and baselines stay as they are.
func TestRecoverIntentKeepsExistingPending(t *testing.T) {
	e := newEnv(t)
	a := e.seedA("x", withExif("A-desc"))
	key := e.checksumA(a)
	if s := e.run(); s.Copied.AtoB != 1 {
		t.Fatalf("%+v", s)
	}
	ps, _, err := e.st.Load("trip")
	if err != nil {
		t.Fatal(err)
	}
	it := ps.Items[key]
	if it == nil || it.Pending == nil {
		t.Fatalf("expected pending after upload: %+v", it)
	}
	want := *it.Pending
	wantOurs, wantMetaB := it.Ours, *it.Meta[B]
	it.Intent = &state.Intent{To: uint8(B), Started: e.clk.Now().Add(-30 * time.Minute), Source: state.AssetRef{ID: a},
		SourceMeta: state.Meta{Description: "intent-desc"}, UploadStarted: true}
	if err := e.st.Save("trip", ps); err != nil {
		t.Fatal(err)
	}
	s := e.run()
	if s.Pending != 1 || s.ItemErrorCount != 0 {
		t.Fatalf("%+v", s)
	}
	got := e.stateItem(key)
	if got.Intent != nil {
		t.Errorf("intent not cleared: %+v", got.Intent)
	}
	if got.Pending == nil || *got.Pending != want {
		t.Errorf("pending changed: %+v, want %+v", got.Pending, want)
	}
	if got.Ours != wantOurs || got.Meta[B] == nil || !reflect.DeepEqual(*got.Meta[B], wantMetaB) {
		t.Errorf("ours/baseline changed: ours=%v meta=%+v", got.Ours, got.Meta[B])
	}
}

// The target changed between snapshot and write (re-GET before PUT): no write,
// `stale` skip, baselines of both sides kept so the change is merged next run.
func TestMetaStaleTargetSkipsWrite(t *testing.T) {
	d2020 := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	d2021 := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	fp := func(v float64) *float64 { return &v }
	ip := func(v int) *int { return &v }
	cases := []struct {
		name       string
		srcEdit    func(x *fakeimmich.FakeAsset)
		concurrent func(x *fakeimmich.FakeAsset)
		check      func(t *testing.T, b fakeimmich.FakeAsset) // B after the follow-up run
	}{
		{"description",
			func(x *fakeimmich.FakeAsset) { d := "from-a"; x.Exif.Description = &d },
			func(x *fakeimmich.FakeAsset) { d := "concurrent"; x.Exif.Description = &d },
			func(t *testing.T, b fakeimmich.FakeAsset) {
				if d := derefS(b.Exif.Description); d != "from-a" { // both changed: winner A
					t.Errorf("B description %q, want from-a", d)
				}
			}},
		{"rating",
			func(x *fakeimmich.FakeAsset) { x.Exif.Rating = ip(3) },
			func(x *fakeimmich.FakeAsset) { x.Exif.Rating = ip(5) },
			func(t *testing.T, b fakeimmich.FakeAsset) {
				if b.Exif.Rating == nil || *b.Exif.Rating != 3 {
					t.Errorf("B rating %v, want 3", b.Exif.Rating)
				}
			}},
		{"location",
			func(x *fakeimmich.FakeAsset) { x.Exif.Latitude, x.Exif.Longitude = fp(10), fp(20) },
			func(x *fakeimmich.FakeAsset) { x.Exif.Latitude, x.Exif.Longitude = fp(30), fp(40) },
			func(t *testing.T, b fakeimmich.FakeAsset) {
				if b.Exif.Latitude == nil || *b.Exif.Latitude != 10 {
					t.Errorf("B lat %v, want 10", b.Exif.Latitude)
				}
			}},
		{"date",
			func(x *fakeimmich.FakeAsset) { x.Exif.DateTimeOriginal = &d2020 },
			func(x *fakeimmich.FakeAsset) { x.Exif.DateTimeOriginal = &d2021 },
			func(t *testing.T, b fakeimmich.FakeAsset) {
				if b.Exif.DateTimeOriginal == nil || !b.Exif.DateTimeOriginal.Equal(d2020) {
					t.Errorf("B date %v, want %v", b.Exif.DateTimeOriginal, d2020)
				}
			}},
		{"visibility",
			func(x *fakeimmich.FakeAsset) { x.Visibility = immich.VisibilityArchive },
			func(x *fakeimmich.FakeAsset) { x.Visibility = immich.VisibilityArchive },
			func(t *testing.T, b fakeimmich.FakeAsset) {
				if b.Visibility != immich.VisibilityArchive {
					t.Errorf("B visibility %v", b.Visibility)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, a, b := pairedWithExif(t)
			key := e.checksumA(a)
			before := e.stateItem(key)
			e.fa.Edit(a, tc.srcEdit)
			var once sync.Once
			hb := &hookImmich{Immich: e.clB, onGet: func(id string) {
				if id == b {
					once.Do(func() { e.fb.Edit(b, tc.concurrent) })
				}
			}}
			e.eng = e.engineWith(e.clA, hb)
			putsBefore := e.countReq(e.fb, "PUT", "/assets/"+b)
			s := e.run()
			if s.Skipped[SkipStale] != 1 || s.MetaUpdates.AtoB != 0 || s.ItemErrorCount != 0 {
				t.Fatalf("%+v", s)
			}
			if n := e.countReq(e.fb, "PUT", "/assets/"+b) - putsBefore; n != 0 {
				t.Fatalf("%d PUTs to the stale target", n)
			}
			after := e.stateItem(key)
			if !reflect.DeepEqual(after.Meta, before.Meta) || after.MetaID != before.MetaID {
				t.Fatalf("baselines changed on a stale skip:\nbefore %+v %+v\nafter  %+v %+v",
					*before.Meta[A], *before.Meta[B], *after.Meta[A], *after.Meta[B])
			}
			e.eng = e.engineWith(e.clA, e.clB)
			if s = e.run(); s.Skipped[SkipStale] != 0 || s.ItemErrorCount != 0 {
				t.Fatalf("follow-up: %+v", s)
			}
			tc.check(t, e.onB("x"))
		})
	}
}

// The target vanished (or became unreadable) between snapshot and write: the
// re-GET returns not found → `stale` skip, no write, no item error, baselines kept.
func TestMetaTargetVanishedBeforeWrite(t *testing.T) {
	e, a, b := pairedWithExif(t)
	key := e.checksumA(a)
	before := e.stateItem(key)
	setDesc(e.fa, a, "from-a")
	e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + b, Status: 404})
	s := e.run()
	if s.Skipped[SkipStale] != 1 || s.ItemErrorCount != 0 || s.MetaUpdates.AtoB != 0 {
		t.Fatalf("%+v", s)
	}
	if n := e.countReq(e.fb, "PUT", "/assets/"+b); n != 0 {
		t.Fatalf("%d PUTs", n)
	}
	if after := e.stateItem(key); !reflect.DeepEqual(after.Meta, before.Meta) {
		t.Fatalf("baselines changed: %+v %+v", *after.Meta[A], *after.Meta[B])
	}
	if s = e.run(); s.MetaUpdates.AtoB != 1 || derefS(e.onB("x").Exif.Description) != "from-a" {
		t.Fatalf("not retried: %+v", s)
	}
}

// The PUT fails: item error, no baseline moves (neither the source's nor the
// target's), so the next run retries and delivers the change.
func TestMetaUpdateFailureKeepsBaseline(t *testing.T) {
	e, a, b := pairedWithExif(t)
	key := e.checksumA(a)
	before := e.stateItem(key)
	setDesc(e.fa, a, "from-a")
	e.fb.Inject(fakeimmich.Fault{Method: "PUT", PathPrefix: "/assets/" + b, Status: 500, Times: 2})
	s := e.run()
	if s.ItemErrorCount != 1 || len(s.ItemErrors) != 1 || s.ItemErrors[0].Op != "meta" || s.ItemErrors[0].Side != B ||
		s.ItemErrors[0].Key != key || s.MetaUpdates.AtoB != 0 {
		t.Fatalf("%+v", s)
	}
	if d := derefS(e.onB("x").Exif.Description); d != "base" {
		t.Fatalf("B description %q", d)
	}
	if after := e.stateItem(key); !reflect.DeepEqual(after.Meta, before.Meta) || after.MetaID != before.MetaID {
		t.Fatalf("baselines changed after failed write: %+v %+v", *after.Meta[A], *after.Meta[B])
	}
	s = e.run()
	if s.MetaUpdates.AtoB != 1 || s.ItemErrorCount != 0 || derefS(e.onB("x").Exif.Description) != "from-a" {
		t.Fatalf("retry: %+v B=%q", s, derefS(e.onB("x").Exif.Description))
	}
	if s = e.run(); s.Mutations != 0 {
		t.Fatalf("not converged: %+v", s)
	}
}

// applyMeta's own ownership guard: a known, not-owned target is never read or
// written, and a partial action does not advance any baseline or clear Pending.
// (The planner already filters not-owned targets; this is the engine backstop.)
func TestApplyMetaNotOwnedTargetSkipped(t *testing.T) {
	e := newEnv(t)
	b := e.seedB("x", withExif("base"))
	r := e.directRun([2]map[string]planner.Member{{}, {b: {Ref: state.AssetRef{ID: b}, Owned: false}}})
	r.ps.Item("k").Pending = &state.Pending{To: uint8(B), TargetID: b}
	nb := state.Meta{Description: "new-base"}
	r.metadataPhase([]planner.Action{{Kind: planner.ActionMeta, Key: "k",
		Updates:    [2]*immich.AssetUpdate{nil, {Description: immich.Some("x")}},
		TargetIDs:  [2]string{"", b},
		NewBase:    [2]*state.Meta{&nb, &nb},
		NewBaseIDs: [2]string{"a", b}, ClearPending: true}})
	if r.sum.Skipped[SkipNotOwner] != 1 || r.sum.ItemErrorCount != 0 {
		t.Fatalf("%+v", r.sum)
	}
	if n := e.countReq(e.fb, "GET", "/assets/"+b) + e.countReq(e.fb, "PUT", "/assets/"+b); n != 0 {
		t.Fatalf("%d requests for a not-owned target", n)
	}
	it := r.ps.Items["k"]
	if it.Meta[A] != nil || it.Meta[B] != nil || it.Pending == nil {
		t.Fatalf("state changed: meta=%+v pending=%+v", it.Meta, it.Pending)
	}
}

// liveOnABStillOnB seeds a live photo on A and an unlinked copy of the still in
// album B (the link_repair situation).
func liveOnABStillOnB(t *testing.T) (e *env, mot, still, dup string) {
	e = newEnv(t)
	mot = e.seedA("motion", func(o *fakeimmich.SeedOpts) {
		o.Type, o.Filename, o.Visibility = "VIDEO", "motion.mp4", immich.VisibilityHidden
	})
	still = e.seedA("still", func(o *fakeimmich.SeedOpts) { o.LivePhotoVideoID = mot })
	dup = e.seedB("still")
	return e, mot, still, dup
}

// A paired still on the target that lost its live-photo link is relinked to
// the motion already on the target (no second motion upload).
func TestLinkRepairRelinksLostLink(t *testing.T) {
	e := newEnv(t)
	mot := e.seedA("motion", func(o *fakeimmich.SeedOpts) {
		o.Type, o.Filename, o.Visibility = "VIDEO", "motion.mp4", immich.VisibilityHidden
	})
	e.seedA("still", func(o *fakeimmich.SeedOpts) { o.LivePhotoVideoID = mot })
	e.settle()
	sb := e.onB("still")
	mb, ok := e.findByFile(e.fb, e.ub, "motion.mp4")
	if !ok || sb.LivePhotoVideoID != mb.ID {
		t.Fatalf("initial copy not linked: %+v", sb)
	}
	e.fb.Edit(sb.ID, func(x *fakeimmich.FakeAsset) { x.LivePhotoVideoID = "" })
	s := e.run()
	if s.Linked.AtoB != 1 || s.ItemErrorCount != 0 || s.Copied.AtoB != 0 {
		t.Fatalf("%+v", s)
	}
	if got := e.onB("still").LivePhotoVideoID; got != mb.ID {
		t.Fatalf("relinked to %q, want existing motion %q", got, mb.ID)
	}
	n := 0
	for _, x := range e.fb.AssetsOf(e.ub) {
		if x.OriginalFileName == "motion.mp4" {
			n++
		}
	}
	if n != 1 || e.inAlbumB(mb.ID) {
		t.Fatalf("motion copies on B = %d, in album = %v", n, e.inAlbumB(mb.ID))
	}
	if s = e.run(); s.Linked.AtoB != 0 || s.Mutations != 0 {
		t.Fatalf("repeat: %+v", s)
	}
}

// The target still turns out not to be ours at write time (re-GET): never
// linked, no motion uploaded, `not_owner` skip.
func TestLinkRepairTargetNotOwnedAtWrite(t *testing.T) {
	e, _, _, dup := liveOnABStillOnB(t)
	carol, _ := e.fb.AddUser("carol")
	var once sync.Once
	hb := &hookImmich{Immich: e.clB, onGet: func(id string) {
		if id == dup {
			once.Do(func() { e.fb.Edit(dup, func(x *fakeimmich.FakeAsset) { x.OwnerID = carol }) })
		}
	}}
	e.eng = e.engineWith(e.clA, hb)
	s := e.run()
	if s.Skipped[SkipNotOwner] != 1 || s.Linked.AtoB != 0 || s.ItemErrorCount != 0 {
		t.Fatalf("%+v", s)
	}
	if a, _ := e.fb.Asset(dup); a.LivePhotoVideoID != "" {
		t.Fatal("linked a still we do not own")
	}
	for _, owner := range []string{e.ub, carol} {
		if _, ok := e.findByFile(e.fb, owner, "motion.mp4"); ok {
			t.Fatal("motion uploaded although the link was skipped")
		}
	}
}

// linkRepair's snapshot ownership guard (engine backstop; the planner already
// filters it): a known, not-owned target is skipped without any request.
func TestLinkRepairNotOwnedMemberSkippedDirect(t *testing.T) {
	e, mot, still, dup := liveOnABStillOnB(t)
	r := e.directRun([2]map[string]planner.Member{
		{still: {Ref: state.AssetRef{ID: still}, Owned: true, LivePhotoVideoID: mot}},
		{dup: {Ref: state.AssetRef{ID: dup}, Owned: false}},
	})
	r.metadataPhase([]planner.Action{{Kind: planner.ActionLinkRepair, Key: "k", From: uint8(A), To: uint8(B),
		Refs: []state.AssetRef{{ID: still}}, TargetID: dup}})
	if r.sum.Skipped[SkipNotOwner] != 1 || r.sum.ItemErrorCount != 0 || r.sum.Linked.AtoB != 0 {
		t.Fatalf("%+v", r.sum)
	}
	if n := len(e.fb.Requests()); n != 0 {
		t.Fatalf("%d requests to B", n)
	}
}

// GetAsset of the target still fails: item error (op link_repair), nothing
// linked; the next run repairs the link.
func TestLinkRepairGetAssetErrorIsItemError(t *testing.T) {
	e, _, _, dup := liveOnABStillOnB(t)
	e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + dup, Status: 500, Times: 2})
	s := e.run()
	if s.ItemErrorCount != 1 || len(s.ItemErrors) != 1 || s.ItemErrors[0].Op != "link_repair" || s.ItemErrors[0].Side != B ||
		s.Linked.AtoB != 0 {
		t.Fatalf("%+v", s)
	}
	if a, _ := e.fb.Asset(dup); a.LivePhotoVideoID != "" {
		t.Fatal("linked despite the failed GET")
	}
	s = e.run()
	mb, ok := e.findByFile(e.fb, e.ub, "motion.mp4")
	if a, _ := e.fb.Asset(dup); s.Linked.AtoB != 1 || !ok || a.LivePhotoVideoID != mb.ID {
		t.Fatalf("not repaired on retry: %+v", s)
	}
}

// The re-GET before a metadata write fails with a non-not-found error: item
// error (op meta), no write, baselines kept, retried next run.
func TestMetaTargetGetErrorIsItemError(t *testing.T) {
	e, a, b := pairedWithExif(t)
	key := e.checksumA(a)
	before := e.stateItem(key)
	setDesc(e.fa, a, "from-a")
	e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + b, Status: 500, Times: 2})
	s := e.run()
	if s.ItemErrorCount != 1 || len(s.ItemErrors) != 1 || s.ItemErrors[0].Op != "meta" || s.ItemErrors[0].Side != B ||
		s.Skipped[SkipStale] != 0 || s.MetaUpdates.AtoB != 0 {
		t.Fatalf("%+v", s)
	}
	if n := e.countReq(e.fb, "PUT", "/assets/"+b); n != 0 {
		t.Fatalf("%d PUTs", n)
	}
	if after := e.stateItem(key); !reflect.DeepEqual(after.Meta, before.Meta) {
		t.Fatalf("baselines changed: %+v %+v", *after.Meta[A], *after.Meta[B])
	}
	if s = e.run(); s.MetaUpdates.AtoB != 1 || derefS(e.onB("x").Exif.Description) != "from-a" {
		t.Fatalf("not retried: %+v", s)
	}
}

// The target still got linked by someone else between snapshot and write: no
// second link, no motion upload, no error.
func TestLinkRepairAlreadyLinkedAtWrite(t *testing.T) {
	e, _, _, dup := liveOnABStillOnB(t)
	var once sync.Once
	hb := &hookImmich{Immich: e.clB, onGet: func(id string) {
		if id == dup {
			once.Do(func() { e.fb.Edit(dup, func(x *fakeimmich.FakeAsset) { x.LivePhotoVideoID = "someone-elses-motion" }) })
		}
	}}
	e.eng = e.engineWith(e.clA, hb)
	puts := e.countReq(e.fb, "PUT", "/assets/"+dup)
	s := e.run()
	if s.Linked.AtoB != 0 || s.ItemErrorCount != 0 || s.Skipped[SkipNotOwner] != 0 {
		t.Fatalf("%+v", s)
	}
	if n := e.countReq(e.fb, "PUT", "/assets/"+dup) - puts; n != 0 {
		t.Fatalf("%d PUTs to an already linked still", n)
	}
	if _, ok := e.findByFile(e.fb, e.ub, "motion.mp4"); ok {
		t.Fatal("motion uploaded for an already linked still")
	}
}

// The link PUT fails: item error (op link_repair); the next run links.
func TestLinkRepairLinkFailureIsItemError(t *testing.T) {
	e, _, _, dup := liveOnABStillOnB(t)
	e.fb.Inject(fakeimmich.Fault{Method: "PUT", PathPrefix: "/assets/" + dup, Status: 500, Times: 2})
	s := e.run()
	if s.ItemErrorCount != 1 || len(s.ItemErrors) != 1 || s.ItemErrors[0].Op != "link_repair" || s.Linked.AtoB != 0 {
		t.Fatalf("%+v", s)
	}
	if a, _ := e.fb.Asset(dup); a.LivePhotoVideoID != "" {
		t.Fatal("linked despite failed PUT")
	}
	s = e.run()
	mb, ok := e.findByFile(e.fb, e.ub, "motion.mp4")
	if a, _ := e.fb.Asset(dup); s.Linked.AtoB != 1 || !ok || a.LivePhotoVideoID != mb.ID {
		t.Fatalf("not repaired on retry: %+v", s)
	}
	n := 0
	for _, x := range e.fb.AssetsOf(e.ub) {
		if x.OriginalFileName == "motion.mp4" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d motion copies on B after retry", n)
	}
}
