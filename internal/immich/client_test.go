package immich_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/immich/fakeimmich"
)

func TestServerInfo(t *testing.T) {
	e := newEnv(t)
	v, err := e.cl.Version(e.ctx)
	if err != nil || v != (immich.Version{Major: 3, Minor: 4, Patch: 1}) {
		t.Fatalf("version %v %v", v, err)
	}
	e.srv.SetVersion(3, 2, 0)
	if v, _ = e.cl.Version(e.ctx); v.Minor != 2 {
		t.Fatalf("version %v", v)
	}
	if id, err := e.cl.MyUserID(e.ctx); err != nil || id != e.uid {
		t.Fatalf("me %q %v", id, err)
	}
	if p, err := e.cl.APIKeyPermissions(e.ctx); err != nil || len(p) != 1 || p[0] != "all" {
		t.Fatalf("perms %v %v", p, err)
	}
	// limited key: perms listed; user.read missing -> 403 Forbidden; /api-keys/me needs none.
	_, key := e.srv.AddUser("bob", "album.read")
	cl := clientFor(t, e.srv.URL(), key)
	if p, err := cl.APIKeyPermissions(e.ctx); err != nil || len(p) != 1 || p[0] != "album.read" {
		t.Fatalf("perms %v %v", p, err)
	}
	_, err = cl.MyUserID(e.ctx)
	var ae *immich.APIError
	if !errors.As(err, &ae) || ae.Status != 403 || immich.KindOf(err) != immich.KindForbidden ||
		!strings.Contains(ae.Message, "user.read") || ae.CorrelationID == "" {
		t.Fatalf("want forbidden, got %v", err)
	}
	// bad key
	bad := clientFor(t, e.srv.URL(), "nope")
	if _, err := bad.GetAsset(e.ctx, "x"); immich.KindOf(err) != immich.KindAuth {
		t.Fatalf("want auth, got %v", err)
	}
}

func TestBaseURLNormalization(t *testing.T) {
	e := newEnv(t)
	for _, suffix := range []string{"", "/", "/api", "/api/"} {
		cl := clientFor(t, e.srv.URL()+suffix, e.key)
		if cl.BaseURL() != e.srv.URL() {
			t.Fatalf("%q -> %q", suffix, cl.BaseURL())
		}
		if _, err := cl.Version(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := immich.New(immich.Config{BaseURL: "nonsense"}); err == nil {
		t.Fatal("want error")
	}
	rs := e.srv.Requests()
	if h := rs[0].Header; h.Get("x-api-key") != e.key || h.Get("Accept") != "application/json" || h.Get("User-Agent") != "immich-ferry" {
		t.Fatalf("headers %v", h)
	}
}

func TestAlbumCRUD(t *testing.T) {
	e := newEnv(t)
	al, err := e.cl.GetAlbum(e.ctx, e.srv.CreateAlbum(e.uid, "Trip"))
	if err != nil || al.Name != "Trip" || len(al.Users) != 1 || al.Users[0] != (immich.AlbumUser{UserID: e.uid, Role: "owner"}) {
		t.Fatalf("%+v %v", al, err)
	}

	// PATCH: only set fields; null description.
	if _, err := e.cl.UpdateAlbumInfo(e.ctx, al.ID, immich.AlbumInfoUpdate{Name: immich.Some("Trip2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cl.UpdateAlbumInfo(e.ctx, al.ID, immich.AlbumInfoUpdate{Description: immich.Null[string]()}); err != nil {
		t.Fatal(err)
	}
	p := reqsTo(e.srv, "PATCH", "/api/albums/"+al.ID)
	jsonEq(t, p[0].Body, `{"albumName":"Trip2"}`)
	jsonEq(t, p[1].Body, `{"description":null}`)
	got, err := e.cl.GetAlbum(e.ctx, al.ID)
	if err != nil || got.Name != "Trip2" || got.Description != "" || got.UpdatedAt.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}

	// Create sends name and description; list returns owned and shared albums with roles.
	created, err := e.cl.CreateAlbum(e.ctx, "New", "desc", nil)
	if err != nil || created.Name != "New" || created.Description != "desc" {
		t.Fatalf("%+v %v", created, err)
	}
	jsonEq(t, reqsTo(e.srv, "POST", "/api/albums")[0].Body, `{"albumName":"New","description":"desc"}`)
	bobID, _ := e.srv.AddUser("bob")
	shared := e.srv.CreateAlbum(bobID, "Bob's")
	e.srv.Share(shared, e.uid, "viewer")
	e.srv.CreateAlbum(bobID, "Hidden")
	list, err := e.cl.ListAlbums(e.ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("%+v %v", list, err)
	}
	roles := map[string]string{}
	for _, a := range list {
		for _, u := range a.Users {
			if u.UserID == e.uid {
				roles[a.Name] = u.Role
			}
		}
	}
	if roles["Trip2"] != "owner" || roles["New"] != "owner" || roles["Bob's"] != "viewer" {
		t.Fatalf("roles %v", roles)
	}
	if q := reqsTo(e.srv, "GET", "/api/albums"); len(q) != 1 || q[0].Query != "" {
		t.Fatalf("list requests %+v", q)
	}

	// Not found maps to NotFound (400 "Not found or no album.read access").
	_, err = e.cl.GetAlbum(e.ctx, "11111111-1111-4111-8111-111111111111")
	var ae *immich.APIError
	if !errors.As(err, &ae) || ae.Status != 400 || immich.KindOf(err) != immich.KindNotFound {
		t.Fatalf("%v", err)
	}
}

func TestCreateAlbumSharedAndListUsers(t *testing.T) {
	e := newEnv(t)
	bobID, _ := e.srv.AddUser("bob")
	users, err := e.cl.ListUsers(e.ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("%+v %v", users, err)
	}
	byEmail := map[string]string{}
	for _, u := range users {
		byEmail[u.Email] = u.ID
	}
	if byEmail["bob@example.test"] != bobID {
		t.Fatalf("users %+v", users)
	}

	// The album is shared in the create request itself; the caller's own id is ignored.
	al, err := e.cl.CreateAlbum(e.ctx, "Shared", "", []immich.AlbumUser{{UserID: bobID, Role: "editor"}, {UserID: e.uid, Role: "viewer"}})
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, reqsTo(e.srv, "POST", "/api/albums")[0].Body,
		`{"albumName":"Shared","description":"","albumUsers":[{"userId":"`+bobID+`","role":"editor"},{"userId":"`+e.uid+`","role":"viewer"}]}`)
	want := []immich.AlbumUser{{UserID: e.uid, Role: "owner"}, {UserID: bobID, Role: "editor"}}
	if !reflect.DeepEqual(al.Users, want) {
		t.Fatalf("users %+v", al.Users)
	}

	// Unknown user: 400, nothing created.
	before := len(e.srv.AlbumIDs())
	_, err = e.cl.CreateAlbum(e.ctx, "Bad", "", []immich.AlbumUser{{UserID: "11111111-1111-4111-8111-111111111111", Role: "editor"}})
	var ae *immich.APIError
	if !errors.As(err, &ae) || ae.Status != 400 || len(e.srv.AlbumIDs()) != before {
		t.Fatalf("%v albums %d", err, len(e.srv.AlbumIDs()))
	}
	// Not retried (POST /albums is not idempotent), even on a 5xx.
	n := len(reqsTo(e.srv, "POST", "/api/albums"))
	_, err = e.cl.CreateAlbum(e.ctx, "Dup", "", []immich.AlbumUser{{UserID: bobID, Role: "editor"}, {UserID: bobID, Role: "viewer"}})
	if err == nil || len(reqsTo(e.srv, "POST", "/api/albums")) != n+1 || len(e.srv.AlbumIDs()) != before {
		t.Fatalf("%v", err)
	}
}

func TestAlbumAddRemoveChunking(t *testing.T) {
	e := newEnv(t)
	alb := e.srv.CreateAlbum(e.uid, "A")
	var ids []string
	for i := 0; i < 2500; i++ {
		ids = append(ids, e.srv.SeedAsset(fakeimmich.SeedOpts{Owner: e.uid, Filename: "a.jpg", Content: []byte(fmt.Sprint("c", i))}))
	}
	before, _ := e.srv.Album(alb)
	res, err := e.cl.AddToAlbum(e.ctx, alb, ids)
	if err != nil || len(res) != 2500 {
		t.Fatalf("%d %v", len(res), err)
	}
	for _, r := range res {
		if !r.Success {
			t.Fatalf("%+v", r)
		}
	}
	if n := len(reqsTo(e.srv, "PUT", "/api/albums/"+alb+"/assets")); n != 3 {
		t.Fatalf("want 3 requests, got %d", n)
	}
	after, _ := e.srv.Album(alb)
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatal("add must bump updatedAt")
	}
	// duplicates reported, not fatal
	res, _ = e.cl.AddToAlbum(e.ctx, alb, ids[:2])
	if res[0].Success || res[0].Error != "duplicate" {
		t.Fatalf("%+v", res)
	}
	// remove: 3 chunks, no updatedAt bump.
	res, err = e.cl.RemoveFromAlbum(e.ctx, alb, ids)
	if err != nil || len(res) != 2500 || len(reqsTo(e.srv, "DELETE", "/api/albums/"+alb+"/assets")) != 3 {
		t.Fatalf("%d %v", len(res), err)
	}
	final, _ := e.srv.Album(alb)
	if len(final.AssetIDs) != 0 || !final.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatal("not removed")
	}
	// empty input -> no requests
	n := len(e.srv.Requests())
	if r, err := e.cl.AddToAlbum(e.ctx, alb, nil); err != nil || r != nil || len(e.srv.Requests()) != n {
		t.Fatal("empty add should be a no-op")
	}
}

func TestAlbumPermissionsRule4(t *testing.T) {
	e := newEnv(t)
	bobID, bobKey := e.srv.AddUser("bob")
	carolID, carolKey := e.srv.AddUser("carol")
	alb := e.srv.CreateAlbum(e.uid, "Shared")
	e.srv.Share(alb, bobID, "editor")
	e.srv.Share(alb, carolID, "viewer")
	mine := e.srv.SeedAsset(fakeimmich.SeedOpts{Owner: e.uid, Filename: "a.jpg", Content: []byte("1")})
	other := e.srv.SeedAsset(fakeimmich.SeedOpts{Owner: e.uid, Filename: "b.jpg", Content: []byte("2")})
	e.srv.AddToAlbum(alb, mine, other)
	bob, carol := clientFor(t, e.srv.URL(), bobKey), clientFor(t, e.srv.URL(), carolKey)

	res, err := carol.RemoveFromAlbum(e.ctx, alb, []string{mine})
	if err != nil || res[0].Success || res[0].Error != "no_permission" {
		t.Fatalf("viewer: %+v %v", res, err)
	}
	// editors can remove any member asset, including the owner's (verified on real v3.4.1)
	res, err = bob.RemoveFromAlbum(e.ctx, alb, []string{mine})
	if err != nil || !res[0].Success {
		t.Fatalf("editor: %+v %v", res, err)
	}
	if ids := e.srv.AlbumAssetIDs(alb); len(ids) != 1 || ids[0] != other {
		t.Fatalf("%v", ids)
	}
	// Asset still exists.
	if a, ok := e.srv.Asset(mine); !ok || a.IsTrashed {
		t.Fatal("asset must remain")
	}
	// no access at all -> 400 not found
	_, eveKey := e.srv.AddUser("eve")
	_, err = clientFor(t, e.srv.URL(), eveKey).RemoveFromAlbum(e.ctx, alb, []string{other})
	if immich.KindOf(err) != immich.KindNotFound {
		t.Fatalf("%v", err)
	}
}

func TestListAlbumAssetsPagingAndFilter(t *testing.T) {
	e := newEnv(t)
	alb := e.srv.CreateAlbum(e.uid, "A")
	var visible []string
	for i := 0; i < 2300; i++ {
		id := e.seed("p.jpg", i)
		e.srv.AddToAlbum(alb, id)
		visible = append(visible, id)
	}
	trashed := e.seed("t.jpg", 5000)
	hidden := e.seed("h.mov", 5001, func(o *fakeimmich.SeedOpts) { o.Visibility = immich.VisibilityHidden })
	archived := e.seed("ar.jpg", 5002, func(o *fakeimmich.SeedOpts) { o.Visibility = immich.VisibilityArchive })
	locked := e.seed("l.jpg", 5003)
	e.srv.AddToAlbum(alb, trashed, hidden, archived, locked)
	e.srv.Trash(trashed)
	e.srv.SetVisibility(locked, immich.VisibilityLocked)
	e.srv.Edit(visible[0], func(a *fakeimmich.FakeAsset) { d := int64(1500); a.DurationMS = &d })

	var pages []int
	e.srv.OnSearchPage(func(p int) { pages = append(pages, p) })
	got, err := e.cl.ListAlbumAssets(e.ctx, alb)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2301 { // 2300 + archived
		t.Fatalf("got %d", len(got))
	}
	if fmt.Sprint(pages) != "[0 1 2]" {
		t.Fatalf("pages %v", pages)
	}
	if got[0].ID != visible[0] || got[0].DurationMS == nil || *got[0].DurationMS != 1500 || got[0].Exif != nil && got[0].Exif.Description != nil {
		t.Fatalf("first %+v", got[0])
	}
	if got[0].Exif == nil {
		t.Log("exif nil for asset without exif (null fields decoded as empty Exif is also ok)")
	}
	seen := map[string]bool{}
	for _, a := range got {
		if seen[a.ID] || a.ID == trashed || a.ID == hidden || a.ID == locked {
			t.Fatalf("unexpected %s", a.ID)
		}
		seen[a.ID] = true
	}
	reqs := reqsTo(e.srv, "POST", "/api/search/metadata")
	if len(reqs) != 3 {
		t.Fatalf("requests %d", len(reqs))
	}
	jsonEq(t, reqs[0].Body, fmt.Sprintf(`{"filter":{"albumIds":{"any":["%s"]},"trashedAt":{"eq":null},"visibility":{"in":["archive","timeline"]}},
		"orderBy":{"field":"fileCreatedAt","direction":"asc"},"withExif":true,"size":1000}`, alb))
	var second map[string]any
	_ = json.Unmarshal(reqs[1].Body, &second)
	if second["cursor"] == nil || second["cursor"] == "" || second["albumIds"] != nil || second["withDeleted"] != nil {
		t.Fatalf("second %v", second)
	}
	if len(second) != 5 {
		t.Fatalf("unexpected keys %v", second)
	}
}

func TestListAlbumAssetsDedupe(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		a := func(id string) string {
			return fmt.Sprintf(`{"id":"%s","ownerId":"o","checksum":"c%s","originalFileName":"f.jpg","fileCreatedAt":"2024-01-01T00:00:00.000Z","fileModifiedAt":"2024-01-01T00:00:00.000Z","type":"IMAGE","duration":null,"visibility":"timeline"}`, id, id)
		}
		switch n {
		case 1:
			fmt.Fprintf(w, `{"assets":{"items":[%s,%s],"nextCursor":"c1"}}`, a("1"), a("2"))
		default:
			fmt.Fprintf(w, `{"assets":{"items":[%s,%s],"nextCursor":null}}`, a("2"), a("3"))
		}
	}))
	defer ts.Close()
	cl := clientFor(t, ts.URL, "k")
	got, err := cl.ListAlbumAssets(context.Background(), "alb")
	if err != nil || len(got) != 3 || got[0].ID != "1" || got[2].ID != "3" || got[0].Exif != nil {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestProbeIncludesTrashedAndHidden(t *testing.T) {
	e := newEnv(t)
	alb := e.srv.CreateAlbum(e.uid, "A")
	tr := e.seed("t.jpg", 1)
	hid := e.seed("h.mov", 2, func(o *fakeimmich.SeedOpts) { o.Visibility = immich.VisibilityHidden })
	lock := e.seed("l.jpg", 3)
	vis := e.seed("v.jpg", 4)
	e.srv.AddToAlbum(alb, tr, hid, lock, vis)
	e.srv.Trash(tr)
	e.srv.SetVisibility(lock, immich.VisibilityLocked)
	var sums []string
	for _, id := range []string{tr, hid, lock, vis} {
		a, _ := e.srv.Asset(id)
		sums = append(sums, a.Checksum)
	}
	for i := 0; i < 600; i++ { // forces 2 chunks of <=500
		sums = append(sums, fmt.Sprintf("bogus%d", i))
	}
	got, err := e.cl.ProbeAlbumAssets(e.ctx, alb, sums)
	if err != nil || len(got) != 3 {
		t.Fatalf("%d %v", len(got), err)
	}
	byID := map[string]immich.Asset{}
	for _, a := range got {
		byID[a.ID] = a
	}
	if !byID[tr].IsTrashed || byID[hid].Visibility != immich.VisibilityHidden || byID[lock].ID != "" {
		t.Fatalf("%+v", byID)
	}
	reqs := reqsTo(e.srv, "POST", "/api/search/metadata")
	if len(reqs) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(reqs))
	}
	var body struct {
		Filter   map[string]json.RawMessage `json:"filter"`
		WithExif bool                       `json:"withExif"`
	}
	_ = json.Unmarshal(reqs[0].Body, &body)
	if _, ok := body.Filter["trashedAt"]; ok || body.Filter["visibility"] != nil || body.Filter["checksum"] == nil || !body.WithExif {
		t.Fatalf("%s", reqs[0].Body)
	}
	var cs struct{ In []string }
	_ = json.Unmarshal(body.Filter["checksum"], &cs)
	if len(cs.In) != 500 {
		t.Fatalf("chunk size %d", len(cs.In))
	}
}

func TestAssetGetUpdate(t *testing.T) {
	e := newEnv(t)
	id := e.seed("a.jpg", 1)
	e.srv.Extract(id)
	a, err := e.cl.GetAsset(e.ctx, id)
	if err != nil || a.Exif == nil || a.Exif.DateTimeOriginal == nil || a.OwnerID != e.uid || a.Checksum == "" || a.Type != "IMAGE" {
		t.Fatalf("%+v %v", a, err)
	}
	lat, lon := 48.5, 2.25
	upd := immich.AssetUpdate{
		Description:      immich.Some("hello"),
		DateTimeOriginal: immich.Some("2023-03-04T05:06:07.000+02:00"),
		Latitude:         immich.Some(lat), Longitude: immich.Some(lon),
		Rating: immich.Some(4), IsFavorite: immich.Some(true), Visibility: immich.Some(immich.VisibilityArchive),
	}
	got, err := e.cl.UpdateAsset(e.ctx, id, upd)
	if err != nil {
		t.Fatal(err)
	}
	if *got.Exif.Description != "hello" || *got.Exif.Rating != 4 || !got.IsFavorite || got.Visibility != immich.VisibilityArchive ||
		*got.Exif.Latitude != lat || *got.Exif.TimeZone != "UTC+2" ||
		!got.Exif.DateTimeOriginal.Equal(time.Date(2023, 3, 4, 3, 6, 7, 0, time.UTC)) {
		t.Fatalf("%+v %+v", got, got.Exif)
	}
	// rating null clears; only set fields are sent.
	got, err = e.cl.UpdateAsset(e.ctx, id, immich.AssetUpdate{Rating: immich.Null[int]()})
	if err != nil || got.Exif.Rating != nil {
		t.Fatalf("%+v %v", got, err)
	}
	reqs := reqsTo(e.srv, "PUT", "/api/assets/"+id)
	jsonEq(t, reqs[1].Body, `{"rating":null}`)
	// rule 7 validation errors -> BadRequest
	for name, u := range map[string]immich.AssetUpdate{
		"rating0":    {Rating: immich.Some(0)},
		"lat only":   {Latitude: immich.Some(1.0)},
		"naive date": {DateTimeOriginal: immich.Some("2023-03-04T05:06:07")},
		"null desc":  {Description: immich.Null[string]()},
	} {
		if _, err := e.cl.UpdateAsset(e.ctx, id, u); immich.KindOf(err) != immich.KindBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	// non-owner cannot PUT (rule 5: 400 not found)
	bobID, bobKey := e.srv.AddUser("bob")
	alb := e.srv.CreateAlbum(e.uid, "S", id)
	e.srv.Share(alb, bobID, "editor")
	bob := clientFor(t, e.srv.URL(), bobKey)
	if _, err := bob.UpdateAsset(e.ctx, id, immich.AssetUpdate{IsFavorite: immich.Some(true)}); immich.KindOf(err) != immich.KindNotFound {
		t.Fatalf("%v", err)
	}
	// rule 9: isFavorite false for non-owner even if favorite
	ba, err := bob.GetAsset(e.ctx, id)
	if err != nil || ba.IsFavorite {
		t.Fatalf("%+v %v", ba, err)
	}
	oa, _ := e.cl.GetAsset(e.ctx, id)
	if !oa.IsFavorite {
		t.Fatal("owner favorite lost")
	}
}

func TestUploadCheckAndRestore(t *testing.T) {
	e := newEnv(t)
	id := e.seed("a.jpg", 1)
	tr := e.seed("b.jpg", 2)
	e.srv.Trash(tr)
	lib := e.seed("c.jpg", 3, func(o *fakeimmich.SeedOpts) { o.LibraryID = "lib" })
	a, _ := e.srv.Asset(id)
	b, _ := e.srv.Asset(tr)
	c, _ := e.srv.Asset(lib)
	res, err := e.cl.UploadCheck(e.ctx, []immich.UploadCheckItem{{ID: "x1", Checksum: a.Checksum}, {ID: "x2", Checksum: b.Checksum},
		{ID: "x3", Checksum: "unknown"}, {ID: "x4", Checksum: c.Checksum}})
	if err != nil || len(res) != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	if res[0].Accept || res[0].Reason != "duplicate" || res[0].AssetID != id || res[0].IsTrashed ||
		res[1].Accept || !res[1].IsTrashed || res[1].AssetID != tr || !res[2].Accept || res[3].Accept || res[3].AssetID != lib {
		t.Fatalf("%+v", res)
	}
	before := e.srv.Mutations()
	if err := e.cl.RestoreFromTrash(e.ctx, []string{tr}); err != nil {
		t.Fatal(err)
	}
	if x, _ := e.srv.Asset(tr); x.IsTrashed {
		t.Fatal("not restored")
	}
	if e.srv.Mutations() != before+1 {
		t.Fatal("mutations") // upload-check is not counted, restore is
	}
	// chunking
	var items []immich.UploadCheckItem
	for i := 0; i < 2100; i++ {
		items = append(items, immich.UploadCheckItem{ID: fmt.Sprint(i), Checksum: fmt.Sprint("z", i)})
	}
	if r, err := e.cl.UploadCheck(e.ctx, items); err != nil || len(r) != 2100 || len(reqsTo(e.srv, "POST", "/api/assets/bulk-upload-check")) != 4 {
		t.Fatalf("%d %v", len(r), err)
	}
}

func TestUploadMultipart(t *testing.T) {
	e := newEnv(t)
	content := []byte(strings.Repeat("jpegdata", 1000))
	f := tmpFile(t, content)
	dur, fav := int64(2500), true
	created := time.Date(2023, 7, 8, 9, 10, 11, 123456789, time.FixedZone("x", 2*3600))
	var got int64
	res, err := e.cl.Upload(e.ctx, immich.UploadRequest{Filename: "IMG 1.jpg", FileCreatedAt: created, FileModifiedAt: t0,
		DurationMS: &dur, IsFavorite: &fav, Visibility: immich.VisibilityArchive, File: f}, func(n int64) { got += n })
	if err != nil || res.Duplicate || res.ID == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if got != int64(len(content)) {
		t.Fatalf("onBytes %d", got)
	}
	reqs := reqsTo(e.srv, "POST", "/api/assets")
	rq := reqs[0]
	if rq.ContentLength <= int64(len(content)) || rq.BodyBytes != rq.ContentLength {
		t.Fatalf("content-length %d body %d", rq.ContentLength, rq.BodyBytes)
	}
	if rq.Header.Get("x-immich-checksum") != "" {
		t.Fatal("must not send checksum header")
	}
	var names []string
	for _, ff := range rq.Form {
		names = append(names, ff.Name)
	}
	if strings.Join(names, ",") != "fileCreatedAt,fileModifiedAt,filename,duration,isFavorite,visibility,assetData" {
		t.Fatalf("order %v", names)
	}
	form := rq.Form
	if form[0].Value != "2023-07-08T09:10:11.123456789+02:00" || form[2].Value != "IMG 1.jpg" || form[3].Value != "2500" ||
		form[4].Value != "true" || form[5].Value != "archive" || form[6].FileName != "IMG 1.jpg" || form[6].FileSize != int64(len(content)) {
		t.Fatalf("%+v", form)
	}
	a, ok := e.srv.Asset(res.ID)
	if !ok || string(a.Content) != string(content) || a.Visibility != immich.VisibilityArchive || !a.IsFavorite ||
		a.OriginalFileName != "IMG 1.jpg" || !a.FileCreatedAt.Equal(created) || a.DurationMS == nil || *a.DurationMS != 2500 {
		t.Fatalf("%+v", a)
	}

	// Duplicate (rule 1): fields ignored.
	f2 := tmpFile(t, content)
	res2, err := e.cl.Upload(e.ctx, immich.UploadRequest{Filename: "other.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: f2}, nil)
	if err != nil || !res2.Duplicate || res2.ID != res.ID {
		t.Fatalf("%+v %v", res2, err)
	}
	if a2, _ := e.srv.Asset(res.ID); a2.OriginalFileName != "IMG 1.jpg" {
		t.Fatal("duplicate must not modify asset")
	}
	// Trashed duplicate still duplicate and stays trashed.
	e.srv.Trash(res.ID)
	res3, err := e.cl.Upload(e.ctx, immich.UploadRequest{Filename: "x.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: f2}, nil)
	if err != nil || !res3.Duplicate || res3.ID != res.ID {
		t.Fatalf("%+v %v", res3, err)
	}
	if a3, _ := e.srv.Asset(res.ID); !a3.IsTrashed {
		t.Fatal("must stay trashed")
	}
	// A different owner uploading identical bytes gets a new asset.
	_, bobKey := e.srv.AddUser("bob")
	res4, err := clientFor(t, e.srv.URL(), bobKey).Upload(e.ctx, immich.UploadRequest{Filename: "x.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: f2}, nil)
	if err != nil || res4.Duplicate || res4.ID == res.ID {
		t.Fatalf("%+v %v", res4, err)
	}
	// Validation: empty file / unknown extension -> BadRequest.
	_, err = e.cl.Upload(e.ctx, immich.UploadRequest{Filename: "e.jpg", FileCreatedAt: t0, FileModifiedAt: t0, File: tmpFile(t, nil)}, nil)
	if immich.KindOf(err) != immich.KindBadRequest {
		t.Fatalf("empty: %v", err)
	}
	_, err = e.cl.Upload(e.ctx, immich.UploadRequest{Filename: "e.xyz", FileCreatedAt: t0, FileModifiedAt: t0, File: tmpFile(t, []byte("x"))}, nil)
	if immich.KindOf(err) != immich.KindBadRequest {
		t.Fatalf("ext: %v", err)
	}
}

func TestUploadLivePhotoLinking(t *testing.T) {
	e := newEnv(t)
	alb := e.srv.CreateAlbum(e.uid, "A")
	mv, err := e.cl.Upload(e.ctx, immich.UploadRequest{Filename: "m.mov", FileCreatedAt: t0, FileModifiedAt: t0,
		Visibility: immich.VisibilityHidden, File: tmpFile(t, []byte("movie"))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.cl.Upload(e.ctx, immich.UploadRequest{Filename: "s.jpg", FileCreatedAt: t0, FileModifiedAt: t0,
		LivePhotoVideoID: mv.ID, File: tmpFile(t, []byte("still"))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reqs := reqsTo(e.srv, "POST", "/api/assets")
	last := reqs[1].Form
	if last[len(last)-2].Name != "livePhotoVideoId" || last[len(last)-2].Value != mv.ID {
		t.Fatalf("%+v", last)
	}
	s, _ := e.srv.Asset(st.ID)
	if s.LivePhotoVideoID != mv.ID {
		t.Fatal("not linked")
	}
	// PUT link hides a timeline motion and does NOT remove it from albums.
	m2 := e.seed("m2.mov", 1, func(o *fakeimmich.SeedOpts) { o.Type = "VIDEO" })
	s2 := e.seed("s2.jpg", 2)
	e.srv.AddToAlbum(alb, s2, m2)
	a, err := e.cl.UpdateAsset(e.ctx, s2, immich.AssetUpdate{LivePhotoVideoID: immich.Some(m2)})
	if err != nil || a.LivePhotoVideoID == nil || *a.LivePhotoVideoID != m2 {
		t.Fatalf("%+v %v", a, err)
	}
	if m, _ := e.srv.Asset(m2); m.Visibility != immich.VisibilityHidden || len(m.AlbumIDs) != 1 {
		t.Fatalf("%+v", m)
	}
	// Link to a non-video -> 400.
	if _, err := e.cl.UpdateAsset(e.ctx, s2, immich.AssetUpdate{LivePhotoVideoID: immich.Some(st.ID)}); immich.KindOf(err) != immich.KindBadRequest {
		t.Fatal("expected error")
	}
}

func TestExtractAutoLink(t *testing.T) {
	e := newEnv(t)
	alb := e.srv.CreateAlbum(e.uid, "A")
	still := e.seed("s.jpg", 1)
	mov := e.seed("s.mov", 1, func(o *fakeimmich.SeedOpts) { o.Type = "VIDEO" })
	e.srv.SetContentID(still, "cid")
	e.srv.SetContentID(mov, "cid")
	e.srv.AddToAlbum(alb, still, mov)
	e.srv.Extract(still)
	m, _ := e.srv.Asset(mov)
	s, _ := e.srv.Asset(still)
	if m.Visibility != immich.VisibilityHidden || len(m.AlbumIDs) != 0 || s.LivePhotoVideoID != mov || s.Exif == nil || s.Exif.DateTimeOriginal == nil {
		t.Fatalf("%+v %+v", m, s)
	}
	list, _ := e.cl.ListAlbumAssets(e.ctx, alb)
	if len(list) != 1 || list[0].ID != still {
		t.Fatalf("%+v", list)
	}
}

func TestAlbumCountRule12(t *testing.T) {
	e := newEnv(t)
	ids := []string{e.seed("a.jpg", 1), e.seed("b.jpg", 2), e.seed("c.jpg", 3), e.seed("d.mov", 4), e.seed("e.jpg", 5)}
	alb := e.srv.CreateAlbum(e.uid, "A", ids...)
	e.srv.Trash(ids[1])
	e.srv.SetVisibility(ids[3], immich.VisibilityHidden)
	e.srv.SetVisibility(ids[4], immich.VisibilityArchive)
	al, err := e.cl.GetAlbum(e.ctx, alb)
	if err != nil || al.AssetCount != 3 {
		t.Fatalf("%+v %v", al, err)
	}
}

func TestSearchRules(t *testing.T) {
	e := newEnv(t)
	alb := e.srv.CreateAlbum(e.uid, "A")
	url := e.srv.URL() + "/api/search/metadata"
	// rule 6: mixing flat + structured -> 400
	st, _ := rawPost(t, url, e.key, fmt.Sprintf(`{"filter":{"albumIds":{"any":["%s"]}},"withExif":true,"isFavorite":true}`, alb))
	if st != 400 {
		t.Fatalf("status %d", st)
	}
	st, _ = rawPost(t, url, e.key, `{"albumIds":["x"],"orderBy":{"field":"fileCreatedAt","direction":"asc"}}`)
	if st != 400 {
		t.Fatalf("status %d", st)
	}
	st, _ = rawPost(t, url, e.key, fmt.Sprintf(`{"filter":{"albumIds":{"any":["%s"]}},"size":1001}`, alb))
	if st != 400 {
		t.Fatalf("size: %d", st)
	}
	st, _ = rawPost(t, url, e.key, fmt.Sprintf(`{"filter":{"albumIds":{"any":["%s"]}},"cursor":"garbage!"}`, alb))
	if st != 400 {
		t.Fatalf("cursor: %d", st)
	}
	st, body := rawPost(t, url, e.key, fmt.Sprintf(`{"filter":{"albumIds":{"any":["%s"]}},"withExif":true}`, alb))
	if st != 200 || !strings.Contains(body, `"nextCursor":null`) {
		t.Fatalf("%d %s", st, body)
	}
	// exifInfo present (null fields) for assets w/o exif; ordering by fileCreatedAt, id.
	a := e.seed("a.jpg", 2)
	b := e.seed("b.jpg", 1)
	e.srv.AddToAlbum(alb, a, b)
	got, _ := e.cl.ListAlbumAssets(e.ctx, alb)
	if len(got) != 2 || got[0].ID != b || got[1].ID != a || got[0].Exif == nil || got[0].Exif.DateTimeOriginal != nil {
		t.Fatalf("%+v", got)
	}
	// non-member cannot search the album
	_, otherKey := e.srv.AddUser("zed")
	if _, err := clientFor(t, e.srv.URL(), otherKey).ListAlbumAssets(e.ctx, alb); immich.KindOf(err) != immich.KindNotFound {
		t.Fatalf("%v", err)
	}
	// unknown route -> 404
	resp, _ := http.Get(e.srv.URL() + "/api/nope")
	if resp.StatusCode != 404 {
		t.Fatalf("%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSharedAlbumSearchAndDownloadAccess(t *testing.T) {
	e := newEnv(t)
	bobID, bobKey := e.srv.AddUser("bob")
	bob := clientFor(t, e.srv.URL(), bobKey)
	still := e.srv.SeedAsset(fakeimmich.SeedOpts{Owner: e.uid, Filename: "s.jpg", Content: []byte("still")})
	mov := e.srv.SeedAsset(fakeimmich.SeedOpts{Owner: e.uid, Filename: "s.mov", Type: "VIDEO", Content: []byte("movie"), Visibility: immich.VisibilityHidden})
	e.srv.Edit(still, func(a *fakeimmich.FakeAsset) { a.LivePhotoVideoID = mov })
	priv := e.srv.SeedAsset(fakeimmich.SeedOpts{Owner: e.uid, Filename: "p.jpg", Content: []byte("private")})
	alb := e.srv.CreateAlbum(e.uid, "S", still)
	e.srv.Share(alb, bobID, "viewer")

	list, err := bob.ListAlbumAssets(e.ctx, alb)
	if err != nil || len(list) != 1 || list[0].OwnerID != e.uid {
		t.Fatalf("%+v %v", list, err)
	}
	var buf strings.Builder
	if n, err := bob.DownloadOriginal(e.ctx, mov, &buf, nil); err != nil || n != 5 || buf.String() != "movie" {
		t.Fatalf("live video download: %d %v", n, err)
	}
	if _, err := bob.DownloadOriginal(e.ctx, priv, &buf, nil); immich.KindOf(err) != immich.KindNotFound {
		t.Fatalf("%v", err)
	}
	e.srv.Trash(still)
	if _, err := bob.DownloadOriginal(e.ctx, still, &buf, nil); immich.KindOf(err) != immich.KindNotFound {
		t.Fatalf("trashed: %v", err)
	}
}

func TestDownloadStreaming(t *testing.T) {
	e := newEnv(t)
	content := []byte(strings.Repeat("0123456789abcdef", 200_000)) // 3.2MB
	id := e.srv.SeedAsset(fakeimmich.SeedOpts{Owner: e.uid, Filename: "v.mp4", Type: "VIDEO", Content: content})
	var sb strings.Builder
	var progress int64
	calls := 0
	n, err := e.cl.DownloadOriginal(e.ctx, id, &sb, func(k int64) { progress += k; calls++ })
	if err != nil || n != int64(len(content)) || sb.String() != string(content) || progress != n || calls < 2 {
		t.Fatalf("%d %v progress=%d calls=%d", n, err, progress, calls)
	}
	// nil onBytes OK
	if _, err := e.cl.DownloadOriginal(e.ctx, id, &strings.Builder{}, nil); err != nil {
		t.Fatal(err)
	}
	// truncated fault -> transient error, partial bytes
	e.srv.Inject(fakeimmich.Fault{Method: "GET", PathPrefix: "/api/assets/" + id + "/original", TruncateAt: 100_000})
	sb.Reset()
	n, err = e.cl.DownloadOriginal(e.ctx, id, &sb, nil)
	if err == nil || immich.KindOf(err) != immich.KindTransient || n != 100_000 {
		t.Fatalf("%d %v kind=%v", n, err, immich.KindOf(err))
	}
	// fault consumed: next works. No internal retry happened for the truncated one.
	if _, err := e.cl.DownloadOriginal(e.ctx, id, &strings.Builder{}, nil); err != nil {
		t.Fatal(err)
	}
	// error status: not found
	if _, err := e.cl.DownloadOriginal(e.ctx, "11111111-1111-4111-8111-111111111111", &sb, nil); immich.KindOf(err) != immich.KindNotFound {
		t.Fatalf("%v", err)
	}
	// status fault isn't retried by download
	before := len(reqsTo(e.srv, "GET", "/api/assets/"+id+"/original"))
	e.srv.Inject(fakeimmich.Fault{PathPrefix: "/api/assets/", Status: 503})
	if _, err := e.cl.DownloadOriginal(e.ctx, id, &sb, nil); immich.KindOf(err) != immich.KindTransient {
		t.Fatalf("%v", err)
	}
	if after := len(reqsTo(e.srv, "GET", "/api/assets/"+id+"/original")); after != before+1 {
		t.Fatalf("download retried: %d -> %d", before, after)
	}
}

func TestFaultsAndMutations(t *testing.T) {
	e := newEnv(t)
	al, err := e.cl.GetAlbum(e.ctx, e.srv.CreateAlbum(e.uid, "X"))
	if err != nil || al.ID == "" {
		t.Fatalf("%v", err)
	}
	e.srv.Inject(fakeimmich.Fault{Method: "PATCH", PathPrefix: "/api/albums", Status: 503, Times: 2, Body: `{"message":"boom"}`})
	name := "Y"
	if _, err := e.cl.UpdateAlbumInfo(e.ctx, al.ID, immich.AlbumInfoUpdate{Name: immich.Some(name)}); err != nil { // 2 faults retried, 3rd ok
		t.Fatalf("%v", err)
	}
	if n := len(reqsTo(e.srv, "PATCH", "/api/albums/"+al.ID)); n != 3 {
		t.Fatalf("attempts %d", n)
	}
	if e.srv.Mutations() != 1 {
		t.Fatalf("mutations %d (faulted requests must not count)", e.srv.Mutations())
	}
	e.srv.Inject(fakeimmich.Fault{PathPrefix: "/api/server", Delay: 30 * time.Millisecond})
	start := time.Now()
	if _, err := e.cl.Version(e.ctx); err != nil || time.Since(start) < 30*time.Millisecond {
		t.Fatalf("delay %v %v", time.Since(start), err)
	}
	// Reads do not count as mutations.
	_, _ = e.cl.GetAlbum(e.ctx, al.ID)
	_, _ = e.cl.ListAlbumAssets(e.ctx, al.ID)
	if e.srv.Mutations() != 1 {
		t.Fatalf("mutations %d", e.srv.Mutations())
	}
}

// ---- retry behaviour against custom handlers ----

func TestRetries(t *testing.T) {
	t.Run("503 twice then success", func(t *testing.T) {
		var n atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) <= 2 {
				w.WriteHeader(503)
				return
			}
			fmt.Fprint(w, `{"major":3,"minor":4,"patch":1}`)
		}))
		defer ts.Close()
		v, err := clientFor(t, ts.URL, "k").Version(context.Background())
		if err != nil || v.Major != 3 || n.Load() != 3 {
			t.Fatalf("%v %v %d", v, err, n.Load())
		}
	})
	t.Run("400 not retried", func(t *testing.T) {
		var n atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.Header().Set("X-Correlation-ID", "cid1")
			w.WriteHeader(400)
			fmt.Fprint(w, `{"message":"Validation failed","errors":[{"path":["x"],"message":"bad x"},{"path":["y"],"message":"bad y"}]}`)
		}))
		defer ts.Close()
		_, err := clientFor(t, ts.URL, "k").Version(context.Background())
		var ae *immich.APIError
		if !errors.As(err, &ae) || n.Load() != 1 || ae.Status != 400 || ae.Message != "Validation failed" ||
			len(ae.Details) != 2 || ae.Details[1] != "bad y" || ae.CorrelationID != "cid1" || ae.Method != "GET" || ae.Path != "/server/version" {
			t.Fatalf("%+v %d", ae, n.Load())
		}
		if immich.KindOf(err) != immich.KindBadRequest {
			t.Fatal(immich.KindOf(err))
		}
	})
	t.Run("exhausted retries returns last error", func(t *testing.T) {
		var n atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1); w.WriteHeader(502) }))
		defer ts.Close()
		_, err := clientFor(t, ts.URL, "k").Version(context.Background())
		if immich.KindOf(err) != immich.KindTransient || n.Load() != 4 { // MaxRetries 3 => 4 attempts
			t.Fatalf("%v %d", err, n.Load())
		}
	})
	t.Run("501 is transient kind but not retried", func(t *testing.T) {
		var n atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1); w.WriteHeader(501) }))
		defer ts.Close()
		_, err := clientFor(t, ts.URL, "k").Version(context.Background())
		if immich.KindOf(err) != immich.KindTransient || n.Load() != 1 {
			t.Fatalf("%v %d", err, n.Load())
		}
	})
	t.Run("Retry-After honored", func(t *testing.T) {
		var n atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				w.Header().Set("Retry-After", "0.2")
				w.WriteHeader(429)
				return
			}
			fmt.Fprint(w, `{"major":3,"minor":4,"patch":1}`)
		}))
		defer ts.Close()
		start := time.Now()
		_, err := clientFor(t, ts.URL, "k").Version(context.Background())
		if el := time.Since(start); err != nil || el < 190*time.Millisecond || n.Load() != 2 {
			t.Fatalf("%v elapsed %v n=%d", err, el, n.Load())
		}
	})
	t.Run("network error retried", func(t *testing.T) {
		var n atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				panic(http.ErrAbortHandler)
			}
			fmt.Fprint(w, `{"major":3,"minor":4,"patch":1}`)
		}))
		defer ts.Close()
		if _, err := clientFor(t, ts.URL, "k").Version(context.Background()); err != nil || n.Load() != 2 {
			t.Fatalf("%v %d", err, n.Load())
		}
	})
	t.Run("per-attempt timeout is transient and retried", func(t *testing.T) {
		var n atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			fmt.Fprint(w, `{"major":3,"minor":4,"patch":1}`)
		}))
		defer ts.Close()
		cl, _ := immich.New(immich.Config{BaseURL: ts.URL, JSONTimeout: 50 * time.Millisecond, BackoffBase: time.Millisecond})
		if _, err := cl.Version(context.Background()); err != nil || n.Load() != 2 {
			t.Fatalf("%v %d", err, n.Load())
		}
	})
	t.Run("parent cancel is KindCanceled without retry", func(t *testing.T) {
		var n atomic.Int32
		started := make(chan struct{}, 1)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			select {
			case started <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		}))
		defer ts.Close()
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-started; cancel() }()
		_, err := clientFor(t, ts.URL, "k").Version(ctx)
		if immich.KindOf(err) != immich.KindCanceled || n.Load() != 1 {
			t.Fatalf("%v kind=%v n=%d", err, immich.KindOf(err), n.Load())
		}
		// already canceled: no request at all
		if _, err := clientFor(t, ts.URL, "k").Version(ctx); immich.KindOf(err) != immich.KindCanceled || n.Load() != 1 {
			t.Fatalf("%v", err)
		}
	})
	t.Run("cancel during backoff", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(503)
		}))
		defer ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := clientFor(t, ts.URL, "k").Version(ctx)
		if immich.KindOf(err) != immich.KindCanceled || time.Since(start) > 5*time.Second {
			t.Fatalf("%v", err)
		}
	})
}

func TestKindOf(t *testing.T) {
	api := func(status int, msg string) error {
		return fmt.Errorf("wrap: %w", &immich.APIError{Status: status, Message: msg})
	}
	cases := []struct {
		name string
		err  error
		want immich.Kind
	}{
		{"nil", nil, immich.KindUnknown},
		{"canceled", context.Canceled, immich.KindCanceled},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), immich.KindCanceled},
		{"eof", fmt.Errorf("x: %w", errors.New("boom")), immich.KindUnknown},
		{"401", api(401, "Invalid API key"), immich.KindAuth},
		{"403", api(403, "Missing required permission: album.read"), immich.KindForbidden},
		{"404", api(404, "nope"), immich.KindNotFound},
		{"400 not found", api(400, "Not found or no album.read access"), immich.KindNotFound},
		{"400 no access", api(400, "no asset.update access"), immich.KindNotFound},
		{"400 quota", api(400, "Quota has been exceeded!"), immich.KindQuota},
		{"400 other", api(400, "Validation failed"), immich.KindBadRequest},
		{"413", api(413, "too big"), immich.KindTooLarge},
		{"422", api(422, "x"), immich.KindBadRequest},
		{"429", api(429, "x"), immich.KindTransient},
		{"500", api(500, "x"), immich.KindTransient},
		{"503", api(503, "x"), immich.KindTransient},
		{"302", api(302, "x"), immich.KindBadRequest},
	}
	for _, c := range cases {
		if got := immich.KindOf(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	// net-level errors
	_, err := (&http.Client{Timeout: time.Second}).Get("http://127.0.0.1:1/")
	if immich.KindOf(err) != immich.KindTransient {
		t.Errorf("conn refused: %v", immich.KindOf(err))
	}
	if immich.KindOf(errors.Join(errors.New("a"), errors.New("b"))) != immich.KindUnknown {
		t.Error("plain error should be unknown")
	}
	for k, s := range map[immich.Kind]string{immich.KindTransient: "transient", immich.KindNotFound: "not_found", immich.KindCanceled: "canceled", immich.Kind(99): "unknown"} {
		if k.String() != s {
			t.Errorf("%d: %s", k, k.String())
		}
	}
}

func TestOptMarshal(t *testing.T) {
	m := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := m(immich.AssetUpdate{}); got != "{}" {
		t.Fatal(got)
	}
	got := m(immich.AssetUpdate{Description: immich.Null[string](), Rating: immich.Some(3), IsFavorite: immich.Some(false),
		Latitude: immich.Some(1.5), Longitude: immich.Some(-2.0), Visibility: immich.Some(immich.VisibilityArchive),
		DateTimeOriginal: immich.Some("2024-01-01T00:00:00Z"), LivePhotoVideoID: immich.Null[string]()})
	want := `{"description":null,"dateTimeOriginal":"2024-01-01T00:00:00Z","latitude":1.5,"longitude":-2,"rating":3,"isFavorite":false,"visibility":"archive","livePhotoVideoId":null}`
	if got != want {
		t.Fatalf("%s", got)
	}
	if got := m(&immich.AssetUpdate{Rating: immich.Some(1)}); got != `{"rating":1}` {
		t.Fatal(got)
	}
	if got := m(immich.AlbumInfoUpdate{Name: immich.Some("n")}); got != `{"albumName":"n"}` {
		t.Fatal(got)
	}
	if got := m(immich.AlbumInfoUpdate{Description: immich.Null[string]()}); got != `{"description":null}` {
		t.Fatal(got)
	}
	if got := m(immich.AlbumInfoUpdate{}); got != "{}" {
		t.Fatal(got)
	}
}
