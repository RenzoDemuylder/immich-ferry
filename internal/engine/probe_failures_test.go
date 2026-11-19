package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
	"immich-ferry/internal/state"
)

// pfHook wraps an Immich client and lets a test intercept individual calls.
// A nil hook passes the call through unchanged.
type pfHook struct {
	Immich
	onProbe  func(ctx context.Context, albumID string, sums []string) ([]immich.Asset, error)
	onRemove func(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error)
	onAdd    func(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error)
}

func (h *pfHook) ProbeAlbumAssets(ctx context.Context, albumID string, sums []string) ([]immich.Asset, error) {
	if h.onProbe != nil {
		return h.onProbe(ctx, albumID, sums)
	}
	return h.Immich.ProbeAlbumAssets(ctx, albumID, sums)
}

func (h *pfHook) RemoveFromAlbum(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error) {
	if h.onRemove != nil {
		return h.onRemove(ctx, albumID, ids)
	}
	return h.Immich.RemoveFromAlbum(ctx, albumID, ids)
}

func (h *pfHook) AddToAlbum(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error) {
	if h.onAdd != nil {
		return h.onAdd(ctx, albumID, ids)
	}
	return h.Immich.AddToAlbum(ctx, albumID, ids)
}

// pfSynced returns an env where asset "x" (seeded on A) has been copied to B.
func pfSynced(t *testing.T) (e *env, xa, xb string) {
	t.Helper()
	e = newEnv(t)
	xa = e.seedA("x")
	s := e.run()
	if s.Copied.AtoB != 1 {
		t.Fatalf("setup copy: %+v", s)
	}
	xb = e.onB("x").ID
	if !e.inAlbumB(xb) {
		t.Fatal("setup: x not in album B")
	}
	return e, xa, xb
}

func (e *env) pfItem(key string) *state.Item {
	e.t.Helper()
	return e.loadState().Items[key]
}

func pfHasItemError(s RunSummary, op string, side Side) (ItemError, bool) {
	for _, ie := range s.ItemErrors {
		if ie.Op == op && ie.Side == side {
			return ie, true
		}
	}
	return ItemError{}, false
}

func (e *env) pfAlbumRemovals(srv *fakeimmich.Server, album string) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Method == "DELETE" && strings.HasSuffix(r.Path, "/albums/"+album+"/assets") {
			n++
		}
	}
	return n
}

// probe.go classifyProbe: the asset is still a member of B's album but hidden
// (not trashed/offline). The snapshot (timeline/archive only) does not see it,
// the probe does: probeHidden -> blocked server_hidden, never unlinked on A.
func TestProbeHiddenMemberBlocksWithoutUnlink(t *testing.T) {
	e, xa, xb := pfSynced(t)
	key := e.checksumA(xa)
	e.fb.SetVisibility(xb, immich.VisibilityHidden)
	if !e.inAlbumB(xb) {
		t.Fatal("setup: hidden asset should stay a member of album B")
	}
	s := e.run()
	if s.Unlinked.BtoA != 0 || s.Skipped[SkipServerHidden] != 1 {
		t.Fatalf("summary %+v", s)
	}
	if !e.inAlbumA(xa) {
		t.Fatal("x wrongly unlinked from A")
	}
	if n := e.pfAlbumRemovals(e.fa, e.albumA); n != 0 {
		t.Fatalf("%d album remove requests on A", n)
	}
	it := e.pfItem(key)
	if it == nil || it.Blocked == nil || it.Blocked.Op != "probe" || it.Blocked.Reason != string(SkipServerHidden) || it.Blocked.Side != uint8(B) {
		t.Fatalf("state item %+v", it)
	}
	// Blocked stays blocked on the next run; still no unlink.
	s = e.run()
	if s.Skipped[SkipBlocked] != 1 || s.Unlinked.BtoA != 0 || !e.inAlbumA(xa) {
		t.Fatalf("second run %+v", s)
	}
}

// probe.go hiddenElsewhere (a): no stored refs on the probe side -> (false, nil)
// without any request. removalsPhase skips such candidates before probing, so
// this is exercised directly.
func TestProbeHiddenElsewhereNoRefsReturnsFalse(t *testing.T) {
	e := newEnv(t)
	ps := state.NewPairState("trip")
	ps.Item("k1").Refs[A] = []state.AssetRef{{ID: "00000000-0000-4000-8000-000000000001", Checksum: "k1"}}
	r := &run{e: e.eng, o: e.opts, ctx: context.Background(), ps: ps}
	before := len(e.fb.Requests())
	for _, key := range []string{"k1", "missing"} {
		h, err := r.hiddenElsewhere(B, key)
		if h || err != nil {
			t.Fatalf("%s: hidden=%v err=%v", key, h, err)
		}
	}
	if n := len(e.fb.Requests()) - before; n != 0 {
		t.Fatalf("%d requests to B", n)
	}
}

// probe.go hiddenElsewhere (b)-(e): the key vanished from B's album and the
// probe finds no member, so hiddenElsewhere decides via upload-check + GET.
func TestProbeHiddenElsewhereOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(e *env, xb string)
		unlinked  bool
		blocked   bool
		probeErr  bool
		noGetOfXb bool
	}{
		{
			name: "trashed match ignored -> unlink",
			setup: func(e *env, xb string) {
				e.fb.RemoveFromAlbum(e.albumB, xb)
				e.fb.Trash(xb)
			},
			unlinked: true, noGetOfXb: true,
		},
		{
			name: "match GET not found ignored -> unlink",
			setup: func(e *env, xb string) {
				e.fb.RemoveFromAlbum(e.albumB, xb)
				e.fb.SetVisibility(xb, immich.VisibilityHidden)
				e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + xb, Status: 400,
					Body: `{"message":"Not found or no asset.read access","statusCode":400,"error":"Bad Request"}`})
			},
			unlinked: true,
		},
		{
			name: "match GET 500 unknown -> no unlink, item error",
			setup: func(e *env, xb string) {
				e.fb.RemoveFromAlbum(e.albumB, xb)
				e.fb.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/assets/" + xb, Status: 500, Times: 2}) // MaxRetries 1: both attempts fail
			},
			probeErr: true,
		},
		{
			name: "live hidden outside album -> blocked",
			setup: func(e *env, xb string) {
				e.fb.SetVisibility(xb, immich.VisibilityHidden)
				e.fb.RemoveFromAlbum(e.albumB, xb)
			},
			blocked: true,
		},
		{
			name: "live visible outside album -> user removal, unlink",
			setup: func(e *env, xb string) {
				e.fb.RemoveFromAlbum(e.albumB, xb)
			},
			unlinked: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, xa, xb := pfSynced(t)
			key := e.checksumA(xa)
			tc.setup(e, xb)
			getsBefore := e.countReq(e.fb, "GET", "/assets/"+xb)
			s := e.run()
			if got := e.inAlbumA(xa); got == tc.unlinked {
				t.Fatalf("in album A=%v, want unlinked=%v; summary %+v", got, tc.unlinked, s)
			}
			if _, ok := e.fa.Asset(xa); !ok {
				t.Fatal("asset deleted on A")
			}
			if _, ok := e.fb.Asset(xb); !ok {
				t.Fatal("asset deleted on B")
			}
			if tc.unlinked != (s.Unlinked.BtoA == 1) {
				t.Fatalf("Unlinked %+v", s.Unlinked)
			}
			if tc.noGetOfXb && e.countReq(e.fb, "GET", "/assets/"+xb) != getsBefore {
				t.Fatal("trashed upload-check match should not be fetched")
			}
			it := e.pfItem(key)
			switch {
			case tc.unlinked:
				if it != nil {
					t.Fatalf("state item kept after unlink: %+v", it)
				}
			case tc.blocked:
				if s.Skipped[SkipServerHidden] != 1 || it == nil || it.Blocked == nil || it.Blocked.Reason != string(SkipServerHidden) || it.Blocked.Side != uint8(B) {
					t.Fatalf("not blocked: summary %+v item %+v", s, it)
				}
				if s.ItemErrorCount != 0 {
					t.Fatalf("unexpected item errors %+v", s.ItemErrors)
				}
			case tc.probeErr:
				if _, ok := pfHasItemError(s, "probe", B); !ok {
					t.Fatalf("no probe item error: %+v", s.ItemErrors)
				}
				if it == nil || it.Failure == nil || it.Failure.Op != "probe" || it.Blocked != nil {
					t.Fatalf("state item %+v", it)
				}
				if n := e.pfAlbumRemovals(e.fa, e.albumA); n != 0 {
					t.Fatalf("%d album remove requests on A", n)
				}
				// Once the GET works again (and the backoff expired) the removal propagates.
				e.clk.Advance(time.Hour)
				s = e.run()
				if s.Unlinked.BtoA != 1 || e.inAlbumA(xa) {
					t.Fatalf("retry did not unlink: %+v", s)
				}
			}
		})
	}
}

// probe.go: the probe request itself fails -> per-candidate item error and
// failure backoff, nothing unlinked; a later run completes the removal.
func TestProbeRequestFailureDoesNotUnlink(t *testing.T) {
	e, xa, xb := pfSynced(t)
	key := e.checksumA(xa)
	e.fb.RemoveFromAlbum(e.albumB, xb)
	calls := 0
	hb := &pfHook{Immich: e.clB, onProbe: func(ctx context.Context, albumID string, sums []string) ([]immich.Asset, error) {
		calls++
		// Fail the real request (beyond the client's retry) so the error is classified realistically.
		e.fb.Inject(fakeimmich.Fault{Method: "POST", PathPrefix: "/search/metadata", Status: 500, Times: 2}) // MaxRetries 1: both attempts fail
		return e.clB.ProbeAlbumAssets(ctx, albumID, sums)
	}}
	e.eng = e.engineWith(e.clA, hb)
	s := e.run()
	if calls != 1 {
		t.Fatalf("probe calls %d", calls)
	}
	if !e.inAlbumA(xa) || s.Unlinked.BtoA != 0 || e.pfAlbumRemovals(e.fa, e.albumA) != 0 {
		t.Fatalf("unlinked despite failed probe: %+v", s)
	}
	ie, ok := pfHasItemError(s, "probe", B)
	if !ok || ie.Key != key || ie.Kind != immich.KindTransient {
		t.Fatalf("item errors %+v", s.ItemErrors)
	}
	if s.Result == ResultOK {
		t.Fatalf("result %s", s.Result)
	}
	it := e.pfItem(key)
	if it == nil || it.Failure == nil || it.Failure.Op != "probe" || !it.InBoth {
		t.Fatalf("state item %+v", it)
	}
	// Within backoff: candidate not retried.
	e.eng = e.engineWith(e.clA, e.clB)
	s = e.run()
	if s.Unlinked.BtoA != 0 || !e.inAlbumA(xa) {
		t.Fatalf("retried within backoff: %+v", s)
	}
	e.clk.Advance(time.Hour)
	s = e.run()
	if s.Unlinked.BtoA != 1 || e.inAlbumA(xa) {
		t.Fatalf("retry did not unlink: %+v", s)
	}
	if _, ok := e.fa.Asset(xa); !ok {
		t.Fatal("asset deleted on A")
	}
}

// probe.go unlinkBatch: the RemoveFromAlbum request fails entirely -> failure
// recorded, item error, asset stays; a later run completes the unlink.
func TestUnlinkRequestFailureRetriedLater(t *testing.T) {
	e, xa, xb := pfSynced(t)
	key := e.checksumA(xa)
	e.fb.RemoveFromAlbum(e.albumB, xb)
	e.fa.Inject(fakeimmich.Fault{Method: "DELETE", PathPrefix: "/albums/" + e.albumA + "/assets", Status: 500, Times: 2}) // MaxRetries 1: both attempts fail
	s := e.run()
	if !e.inAlbumA(xa) || s.Unlinked.BtoA != 0 {
		t.Fatalf("summary %+v", s)
	}
	ie, ok := pfHasItemError(s, "unlink", A)
	if !ok || ie.Key != key {
		t.Fatalf("item errors %+v", s.ItemErrors)
	}
	it := e.pfItem(key)
	if it == nil || it.Failure == nil || it.Failure.Op != "unlink" {
		t.Fatalf("state item %+v", it)
	}
	e.clk.Advance(time.Hour)
	s = e.run()
	if s.Unlinked.BtoA != 1 || e.inAlbumA(xa) || s.ItemErrorCount != 0 {
		t.Fatalf("retry: %+v", s)
	}
	if _, ok := e.fa.Asset(xa); !ok {
		t.Fatal("asset deleted on A")
	}
	if e.pfItem(key) != nil {
		t.Fatal("state item kept after unlink")
	}
}

// probe.go unlinkBatch per-id results.
func TestUnlinkPerIDResults(t *testing.T) {
	cases := []struct {
		name    string
		res     func(id string) []immich.BulkIDResult
		deleted bool // state item dropped (treated as unlinked)
		blocked bool
		errMsg  string
	}{
		{name: "not_found counts as done", res: func(id string) []immich.BulkIDResult {
			return []immich.BulkIDResult{{ID: id, Error: "not_found"}}
		}, deleted: true},
		{name: "no_permission blocks", res: func(id string) []immich.BulkIDResult {
			return []immich.BulkIDResult{{ID: id, Error: "no_permission"}}
		}, blocked: true},
		{name: "missing result is an error", res: func(id string) []immich.BulkIDResult {
			return []immich.BulkIDResult{{ID: "00000000-0000-4000-8000-0000000000ff", Success: true}}
		}, errMsg: "missing result"},
		{name: "unknown error string is an error", res: func(id string) []immich.BulkIDResult {
			return []immich.BulkIDResult{{ID: id, Error: "unknown"}}
		}, errMsg: "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, xa, xb := pfSynced(t)
			key := e.checksumA(xa)
			e.fb.RemoveFromAlbum(e.albumB, xb)
			var got []string
			ha := &pfHook{Immich: e.clA, onRemove: func(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error) {
				got = append(got, ids...)
				return tc.res(xa), nil
			}}
			e.eng = e.engineWith(ha, e.clB)
			s := e.run()
			if len(got) != 1 || got[0] != xa {
				t.Fatalf("remove ids %v", got)
			}
			it := e.pfItem(key)
			switch {
			case tc.deleted:
				if it != nil || s.Unlinked.BtoA != 1 || s.ItemErrorCount != 0 {
					t.Fatalf("summary %+v item %+v", s, it)
				}
			case tc.blocked:
				if it == nil || it.Blocked == nil || it.Blocked.Op != "unlink" || it.Blocked.Reason != "no_permission" || it.Blocked.Side != uint8(A) {
					t.Fatalf("item %+v", it)
				}
				if s.Skipped[SkipBlocked] != 1 || s.Unlinked.BtoA != 0 || s.ItemErrorCount != 0 {
					t.Fatalf("summary %+v", s)
				}
			default:
				ie, ok := pfHasItemError(s, "unlink", A)
				if !ok || ie.Key != key || !strings.Contains(ie.Msg, tc.errMsg) {
					t.Fatalf("item errors %+v", s.ItemErrors)
				}
				if s.Unlinked.BtoA != 0 || it == nil || it.Failure == nil || it.Failure.Op != "unlink" || it.Blocked != nil {
					t.Fatalf("summary %+v item %+v", s, it)
				}
			}
			// The fake never removed anything (the hook answered); the asset is intact.
			if _, ok := e.fa.Asset(xa); !ok || !e.inAlbumA(xa) {
				t.Fatal("asset changed on A")
			}
		})
	}
}

// copy.go flushAdds: the whole AddToAlbum request fails -> album_add item
// error; the next run adds the already uploaded asset without re-uploading.
func TestAlbumAddRequestFailureRetriedWithoutReupload(t *testing.T) {
	e := newEnv(t)
	xa := e.seedA("x")
	key := e.checksumA(xa)
	e.fb.Inject(fakeimmich.Fault{Method: "PUT", PathPrefix: "/albums/" + e.albumB + "/assets", Status: 500, Times: 2}) // MaxRetries 1: both attempts fail
	s := e.run()
	ie, ok := pfHasItemError(s, "album_add", B)
	if !ok || ie.Key != key {
		t.Fatalf("item errors %+v", s.ItemErrors)
	}
	if s.Copied.AtoB != 0 {
		t.Fatalf("copied %+v", s.Copied)
	}
	xb := e.onB("x").ID
	if e.inAlbumB(xb) {
		t.Fatal("unexpectedly in album B")
	}
	uploads := e.countReq(e.fb, "POST", "/assets")
	if uploads != 1 {
		t.Fatalf("uploads %d", uploads)
	}
	it := e.pfItem(key)
	if it == nil || it.Failure == nil {
		t.Fatalf("state item %+v", it)
	}
	e.clk.Advance(time.Hour)
	s = e.run()
	if s.Copied.AtoB != 1 || !e.inAlbumB(xb) || s.ItemErrorCount != 0 {
		t.Fatalf("retry: %+v", s)
	}
	if n := e.countReq(e.fb, "POST", "/assets"); n != uploads {
		t.Fatalf("re-uploaded: %d uploads", n)
	}
	if n := len(e.fb.AssetsOf(e.ub)); n != 1 {
		t.Fatalf("%d assets on B", n)
	}
	it = e.pfItem(key)
	if it == nil || it.Failure != nil || it.Ours[B] != xb {
		t.Fatalf("state item after retry %+v", it)
	}
}

// copy.go flushAdds per-id results -> item error with the server's reason.
func TestAlbumAddPerIDResults(t *testing.T) { pfAlbumAddPerIDResults(t, false) }

// copy.go flushAdds classifies no_permission/not_found into a kindError, but
// immich.KindOf only knows *immich.APIError and immich's own unexported kindErr,
// so the classification is lost and every per-id failure reports KindUnknown.
func TestAlbumAddPerIDResultKinds(t *testing.T) {
	pfAlbumAddPerIDResults(t, true)
}

func pfAlbumAddPerIDResults(t *testing.T, checkKind bool) {
	cases := []struct {
		name string
		res  func(id string) []immich.BulkIDResult
		kind immich.Kind
		msg  string
	}{
		{"no_permission", func(id string) []immich.BulkIDResult {
			return []immich.BulkIDResult{{ID: id, Error: "no_permission"}}
		}, immich.KindForbidden, "no_permission"},
		{"not_found", func(id string) []immich.BulkIDResult {
			return []immich.BulkIDResult{{ID: id, Error: "not_found"}}
		}, immich.KindNotFound, "not_found"},
		{"missing result", func(id string) []immich.BulkIDResult {
			return nil
		}, immich.KindUnknown, "missing result"},
		{"unknown error", func(id string) []immich.BulkIDResult {
			return []immich.BulkIDResult{{ID: id, Error: "unknown"}}
		}, immich.KindUnknown, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			xa := e.seedA("x")
			key := e.checksumA(xa)
			hb := &pfHook{Immich: e.clB, onAdd: func(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error) {
				if len(ids) != 1 {
					t.Errorf("add ids %v", ids)
					return nil, nil
				}
				return tc.res(ids[0]), nil
			}}
			e.eng = e.engineWith(e.clA, hb)
			s := e.run()
			ie, ok := pfHasItemError(s, "album_add", B)
			if !ok || ie.Key != key || !strings.Contains(ie.Msg, tc.msg) {
				t.Fatalf("item errors %+v", s.ItemErrors)
			}
			if checkKind && ie.Kind != tc.kind {
				t.Fatalf("kind %s, want %s", ie.Kind, tc.kind)
			}
			if s.Copied.AtoB != 0 || s.Result == ResultOK {
				t.Fatalf("summary %+v", s)
			}
			if e.inAlbumB(e.onB("x").ID) {
				t.Fatal("in album B")
			}
			it := e.pfItem(key)
			if it == nil || it.Failure == nil || it.InBoth {
				t.Fatalf("state item %+v", it)
			}
		})
	}
}

// albuminfo.go: album B changed between snapshot and the album-info write ->
// write skipped (stale), the concurrent user edit survives.
func TestAlbumInfoChangedSinceSnapshotSkipsWrite(t *testing.T) {
	e, xa, xb := pfSynced(t)
	e.run()
	ctx := context.Background()
	if _, err := e.clA.UpdateAlbumInfo(ctx, e.albumA, immich.AlbumInfoUpdate{Name: immich.Some("Renamed A")}); err != nil {
		t.Fatal(err)
	}
	// A removal on A makes the removals phase (before album info) unlink on B;
	// the hook edits album B right after that, i.e. after B's snapshot.
	e.fa.RemoveFromAlbum(e.albumA, xa)
	hb := &pfHook{Immich: e.clB, onRemove: func(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error) {
		res, err := e.clB.RemoveFromAlbum(ctx, albumID, ids)
		e.fb.EditAlbum(e.albumB, func(al *fakeimmich.FakeAlbum) { al.Name = "User edit B" })
		return res, err
	}}
	e.eng = e.engineWith(e.clA, hb)
	patchesBefore := e.countReq(e.fb, "PATCH", "/albums/"+e.albumB)
	s := e.run()
	if s.Unlinked.AtoB != 1 || e.inAlbumB(xb) {
		t.Fatalf("setup unlink: %+v", s)
	}
	if n := e.countReq(e.fb, "PATCH", "/albums/"+e.albumB); n != patchesBefore {
		t.Fatalf("album B patched despite concurrent change (%d)", n-patchesBefore)
	}
	if s.Skipped[SkipStale] < 1 || s.AlbumInfoUpdated.AtoB != 0 {
		t.Fatalf("summary %+v", s)
	}
	if al, _ := e.fb.Album(e.albumB); al.Name != "User edit B" {
		t.Fatalf("user edit overwritten: %q", al.Name)
	}
}

// albuminfo.go: UpdateAlbumInfo fails -> item error, baseline not advanced,
// retried on the next run.
func TestAlbumInfoUpdateFailureRetried(t *testing.T) {
	e, _, _ := pfSynced(t)
	e.run()
	base := e.loadState().AlbumInfo
	if base[A] == nil || base[B] == nil || base[B].Name != "Trip" {
		t.Fatalf("setup baselines %+v %+v", base[A], base[B])
	}
	ctx := context.Background()
	if _, err := e.clA.UpdateAlbumInfo(ctx, e.albumA, immich.AlbumInfoUpdate{Name: immich.Some("Renamed A")}); err != nil {
		t.Fatal(err)
	}
	e.fb.Inject(fakeimmich.Fault{Method: "PATCH", PathPrefix: "/albums/" + e.albumB, Status: 500, Times: 2}) // MaxRetries 1: both attempts fail
	s := e.run()
	if _, ok := pfHasItemError(s, "albuminfo", B); !ok {
		t.Fatalf("item errors %+v", s.ItemErrors)
	}
	if s.AlbumInfoUpdated.AtoB != 0 || s.Result == ResultOK {
		t.Fatalf("summary %+v", s)
	}
	after := e.loadState().AlbumInfo
	if after[A] == nil || after[A].Name != "Trip" || after[B] == nil || after[B].Name != "Trip" {
		t.Fatalf("baseline advanced: A=%+v B=%+v", after[A], after[B])
	}
	if al, _ := e.fb.Album(e.albumB); al.Name != "Trip" {
		t.Fatalf("B renamed despite failure: %q", al.Name)
	}
	s = e.run()
	if s.AlbumInfoUpdated.AtoB != 1 || s.ItemErrorCount != 0 {
		t.Fatalf("retry %+v", s)
	}
	if al, _ := e.fb.Album(e.albumB); al.Name != "Renamed A" {
		t.Fatalf("rename not applied: %q", al.Name)
	}
	after = e.loadState().AlbumInfo
	if after[A] == nil || after[A].Name != "Renamed A" || after[B] == nil || after[B].Name != "Renamed A" {
		t.Fatalf("baseline after retry: A=%+v B=%+v", after[A], after[B])
	}
}
