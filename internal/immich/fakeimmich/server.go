// Package fakeimmich is an in-memory fake Immich server (httptest) for tests.
//
// It mimics the behaviour of Immich v3.4.1 REST endpoints used by the sync
// client, including the quirks listed in docs/DESIGN.md Appendix B. Owner
// arguments/fields are user ids as returned by AddUser.
package fakeimmich

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"immich-ferry/internal/immich"
)

type user struct {
	id, name, email, key string
	perms                []string
}

// Server is the fake Immich instance.
type Server struct {
	tb  testing.TB
	srv *httptest.Server

	mu        sync.Mutex
	users     map[string]*user // by id
	keys      map[string]*user // by api key
	assets    map[string]*FakeAsset
	order     []string // asset ids in creation order
	albums    map[string]*FakeAlbum
	albumSeq  []string // album ids in creation order
	version   [3]int
	faults    []*faultState
	searchFn  func(page int)
	reqs      []*RecordedRequest
	mutations int
	lastTick  time.Time
}

// New starts a fake server; tb.Cleanup closes it.
func New(tb testing.TB) *Server {
	s := &Server{
		tb:      tb,
		users:   map[string]*user{},
		keys:    map[string]*user{},
		assets:  map[string]*FakeAsset{},
		albums:  map[string]*FakeAlbum{},
		version: [3]int{3, 4, 1},
	}
	s.srv = httptest.NewServer(s.handler())
	tb.Cleanup(s.srv.Close)
	return s
}

// URL returns the base URL (without /api).
func (s *Server) URL() string { return s.srv.URL }

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// tick returns a strictly increasing UTC timestamp (ms precision). Caller holds mu.
func (s *Server) tick() time.Time {
	t := time.Now().UTC().Truncate(time.Millisecond)
	if !t.After(s.lastTick) {
		t = s.lastTick.Add(time.Millisecond)
	}
	s.lastTick = t
	return t
}

// AddUser adds a user and API key; perms default to ["all"].
func (s *Server) AddUser(name string, perms ...string) (userID, apiKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(perms) == 0 {
		perms = []string{"all"}
	}
	var kb [12]byte
	_, _ = rand.Read(kb[:])
	u := &user{id: newUUID(), name: name, email: name + "@example.test", key: "key-" + hex.EncodeToString(kb[:]), perms: perms}
	s.users[u.id] = u
	s.keys[u.key] = u
	return u.id, u.key
}

// SetVersion sets the reported server version.
func (s *Server) SetVersion(major, minor, patch int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = [3]int{major, minor, patch}
}

// SeedOpts describes an asset to seed.
type SeedOpts struct {
	Owner, Filename  string
	Content          []byte
	Type             string // IMAGE|VIDEO
	Visibility       immich.Visibility
	FileCreatedAt    time.Time
	LibraryID        string // external: checksum = sha1("path:"+filename)
	Exif             *immich.Exif
	LivePhotoVideoID string
}

// FakeAsset mirrors immich.Asset plus fake-only state.
type FakeAsset struct {
	ID, OwnerID, Checksum string
	LibraryID             string
	OriginalFileName      string
	FileCreatedAt         time.Time
	FileModifiedAt        time.Time
	Type                  string
	DurationMS            *int64
	LivePhotoVideoID      string
	IsFavorite            bool
	IsTrashed             bool
	IsOffline             bool
	Visibility            immich.Visibility
	Exif                  *immich.Exif
	UpdatedAt             time.Time

	Content   []byte
	ContentID string
	AlbumIDs  []string
}

// FakeAlbum mirrors immich.Album plus members.
type FakeAlbum struct {
	ID, Name, Description string
	OwnerID               string
	UpdatedAt             time.Time
	Users                 []immich.AlbumUser // excludes owner
	AssetIDs              []string
	CreatedAt             time.Time
}

func checksumOf(content []byte, libraryID, filename string) string {
	var sum [sha1.Size]byte
	if libraryID != "" {
		sum = sha1.Sum([]byte("path:" + filename))
	} else {
		sum = sha1.Sum(content)
	}
	return base64.StdEncoding.EncodeToString(sum[:])
}

func cloneExif(e *immich.Exif) *immich.Exif {
	if e == nil {
		return nil
	}
	c := *e
	if e.Description != nil {
		v := *e.Description
		c.Description = &v
	}
	if e.DateTimeOriginal != nil {
		v := *e.DateTimeOriginal
		c.DateTimeOriginal = &v
	}
	if e.TimeZone != nil {
		v := *e.TimeZone
		c.TimeZone = &v
	}
	if e.Latitude != nil {
		v := *e.Latitude
		c.Latitude = &v
	}
	if e.Longitude != nil {
		v := *e.Longitude
		c.Longitude = &v
	}
	if e.Rating != nil {
		v := *e.Rating
		c.Rating = &v
	}
	if e.FileSizeInByte != nil {
		v := *e.FileSizeInByte
		c.FileSizeInByte = &v
	}
	return &c
}

// SeedAsset creates an asset and returns its id.
func (s *Server) SeedAsset(o SeedOpts) (id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &FakeAsset{
		ID: newUUID(), OwnerID: o.Owner, OriginalFileName: o.Filename, Content: o.Content,
		Type: o.Type, Visibility: o.Visibility, FileCreatedAt: o.FileCreatedAt.UTC(),
		LibraryID: o.LibraryID, Exif: cloneExif(o.Exif), LivePhotoVideoID: o.LivePhotoVideoID,
	}
	if a.Type == "" {
		a.Type = "IMAGE"
	}
	if a.Visibility == "" {
		a.Visibility = immich.VisibilityTimeline
	}
	now := s.tick()
	if a.FileCreatedAt.IsZero() {
		a.FileCreatedAt = now
	}
	a.FileModifiedAt = a.FileCreatedAt
	a.UpdatedAt = now
	a.Checksum = checksumOf(o.Content, o.LibraryID, o.Filename)
	if a.Exif != nil && a.Exif.FileSizeInByte == nil {
		n := int64(len(o.Content))
		a.Exif.FileSizeInByte = &n
	}
	s.assets[a.ID] = a
	s.order = append(s.order, a.ID)
	return a.ID
}

// CreateAlbum creates an album owned by owner.
func (s *Server) CreateAlbum(owner, name string, assetIDs ...string) (id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	al := s.createAlbumLocked(owner, name, "")
	for _, aid := range assetIDs {
		s.addToAlbumLocked(al, aid)
	}
	return al.ID
}

func (s *Server) createAlbumLocked(owner, name, desc string) *FakeAlbum {
	now := s.tick()
	al := &FakeAlbum{ID: newUUID(), Name: name, Description: desc, OwnerID: owner, CreatedAt: now, UpdatedAt: now}
	s.albums[al.ID] = al
	s.albumSeq = append(s.albumSeq, al.ID)
	return al
}

func (s *Server) addToAlbumLocked(al *FakeAlbum, id string) bool {
	for _, x := range al.AssetIDs {
		if x == id {
			return false
		}
	}
	al.AssetIDs = append(al.AssetIDs, id)
	al.UpdatedAt = s.tick()
	return true
}

func (s *Server) removeFromAlbumLocked(al *FakeAlbum, id string) bool {
	for i, x := range al.AssetIDs {
		if x == id {
			al.AssetIDs = append(al.AssetIDs[:i:i], al.AssetIDs[i+1:]...)
			return true
		}
	}
	return false
}

func (s *Server) removeFromAllAlbumsLocked(id string) {
	for _, al := range s.albums {
		s.removeFromAlbumLocked(al, id)
	}
}

// Share shares an album with userID; role is editor|viewer.
func (s *Server) Share(albumID, userID, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	al := s.albums[albumID]
	if al == nil {
		s.tb.Fatalf("fakeimmich: Share: unknown album %s", albumID)
		return
	}
	for i := range al.Users {
		if al.Users[i].UserID == userID {
			al.Users[i].Role = role
			return
		}
	}
	al.Users = append(al.Users, immich.AlbumUser{UserID: userID, Role: role})
}

// EditAlbum mutates an album directly (no updatedAt bump).
func (s *Server) EditAlbum(id string, f func(al *FakeAlbum)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	al := s.albums[id]
	if al == nil {
		s.tb.Fatalf("fakeimmich: EditAlbum: unknown album %s", id)
		return
	}
	f(al)
}

// DeleteAlbum deletes an album (its assets are kept, as in Immich).
func (s *Server) DeleteAlbum(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.albums, id)
	for i, x := range s.albumSeq {
		if x == id {
			s.albumSeq = append(s.albumSeq[:i:i], s.albumSeq[i+1:]...)
			break
		}
	}
}

// LeaveAlbum removes userID from the album's members (a member leaving it).
func (s *Server) LeaveAlbum(albumID, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	al := s.albums[albumID]
	if al == nil {
		s.tb.Fatalf("fakeimmich: LeaveAlbum: unknown album %s", albumID)
		return
	}
	for i, x := range al.Users {
		if x.UserID == userID {
			al.Users = append(al.Users[:i:i], al.Users[i+1:]...)
			return
		}
	}
}

// purgeLocked deletes an asset permanently. Caller holds mu.
func (s *Server) purgeLocked(id string) {
	s.removeFromAllAlbumsLocked(id)
	delete(s.assets, id)
	for i, x := range s.order {
		if x == id {
			s.order = append(s.order[:i:i], s.order[i+1:]...)
			break
		}
	}
}

// AddToAlbum adds assets directly (bumps updatedAt).
func (s *Server) AddToAlbum(albumID string, ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if al := s.albums[albumID]; al != nil {
		for _, id := range ids {
			s.addToAlbumLocked(al, id)
		}
	}
}

// RemoveFromAlbum removes assets directly (does not bump updatedAt).
func (s *Server) RemoveFromAlbum(albumID string, ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if al := s.albums[albumID]; al != nil {
		for _, id := range ids {
			s.removeFromAlbumLocked(al, id)
		}
	}
}

// Trash marks assets trashed.
func (s *Server) Trash(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if a := s.assets[id]; a != nil {
			a.IsTrashed = true
			a.UpdatedAt = s.tick()
		}
	}
}

// Restore un-trashes assets.
func (s *Server) Restore(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if a := s.assets[id]; a != nil {
			a.IsTrashed = false
			a.UpdatedAt = s.tick()
		}
	}
}

// SetOffline marks an asset offline or online.
func (s *Server) SetOffline(id string, off bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.assets[id]; a != nil {
		a.IsOffline = off
		a.UpdatedAt = s.tick()
	}
}

// SetVisibility sets visibility; locked removes the asset from all albums.
func (s *Server) SetVisibility(id string, v immich.Visibility) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setVisibilityLocked(id, v)
}

func (s *Server) setVisibilityLocked(id string, v immich.Visibility) {
	if a := s.assets[id]; a != nil {
		a.Visibility = v
		a.UpdatedAt = s.tick()
		if v == immich.VisibilityLocked {
			s.removeFromAllAlbumsLocked(id)
		}
	}
}

// Edit mutates an asset directly and bumps updatedAt. f must not call other
// Server methods (the server lock is held).
func (s *Server) Edit(id string, f func(a *FakeAsset)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.assets[id]; a != nil {
		f(a)
		a.ID = id
		a.UpdatedAt = s.tick()
	}
}

// Extract simulates metadata extraction: fills exif dateTimeOriginal (fallback
// fileCreatedAt) and auto-links still+video sharing ContentID (hides motion and
// removes it from all albums).
func (s *Server) Extract(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.assets[id]
	if a == nil {
		return
	}
	if a.Exif == nil {
		a.Exif = &immich.Exif{}
	}
	if a.Exif.DateTimeOriginal == nil {
		t := a.FileCreatedAt
		a.Exif.DateTimeOriginal = &t
	}
	if a.Exif.TimeZone == nil {
		tz := "UTC+0"
		a.Exif.TimeZone = &tz
	}
	if a.Exif.FileSizeInByte == nil {
		n := int64(len(a.Content))
		a.Exif.FileSizeInByte = &n
	}
	a.UpdatedAt = s.tick()
	if a.ContentID == "" {
		return
	}
	var still, video *FakeAsset
	for _, oid := range s.order {
		o := s.assets[oid]
		if o.OwnerID != a.OwnerID || o.ContentID != a.ContentID {
			continue
		}
		if o.Type == "VIDEO" && video == nil {
			video = o
		} else if o.Type == "IMAGE" && still == nil {
			still = o
		}
	}
	if still != nil && video != nil {
		still.LivePhotoVideoID = video.ID
		still.UpdatedAt = s.tick()
		video.Visibility = immich.VisibilityHidden
		video.UpdatedAt = s.tick()
		s.removeFromAllAlbumsLocked(video.ID)
	}
}

// SetContentID sets the live-photo ContentIdentifier of an asset.
func (s *Server) SetContentID(id, cid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.assets[id]; a != nil {
		a.ContentID = cid
	}
}

func (s *Server) copyAssetLocked(a *FakeAsset) FakeAsset {
	c := *a
	c.Exif = cloneExif(a.Exif)
	c.AlbumIDs = nil
	for _, aid := range s.albumSeq {
		for _, x := range s.albums[aid].AssetIDs {
			if x == a.ID {
				c.AlbumIDs = append(c.AlbumIDs, aid)
				break
			}
		}
	}
	return c
}

// Asset returns a copy of an asset.
func (s *Server) Asset(id string) (FakeAsset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.assets[id]
	if a == nil {
		return FakeAsset{}, false
	}
	return s.copyAssetLocked(a), true
}

// Album returns a copy of an album.
func (s *Server) Album(id string) (FakeAlbum, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	al := s.albums[id]
	if al == nil {
		return FakeAlbum{}, false
	}
	c := *al
	c.Users = append([]immich.AlbumUser(nil), al.Users...)
	c.AssetIDs = append([]string(nil), al.AssetIDs...)
	return c, true
}

// AlbumIDs returns the ids of all albums (creation order).
func (s *Server) AlbumIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.albumSeq...)
}

// AlbumAssetIDs returns the asset ids in an album.
func (s *Server) AlbumAssetIDs(albumID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if al := s.albums[albumID]; al != nil {
		return append([]string(nil), al.AssetIDs...)
	}
	return nil
}

// AssetsOf returns all assets owned by owner (creation order).
func (s *Server) AssetsOf(owner string) []FakeAsset {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []FakeAsset
	for _, id := range s.order {
		if a := s.assets[id]; a.OwnerID == owner {
			out = append(out, s.copyAssetLocked(a))
		}
	}
	return out
}

// OnSearchPage registers a hook called before serving each search page. page is
// the 0-based page index (offset/size) about to be served. The hook runs
// without the server lock held, so it may call Server methods.
func (s *Server) OnSearchPage(fn func(page int)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchFn = fn
}

// Mutations counts mutating requests (POST/PUT/PATCH/DELETE except search and
// bulk-upload-check) that were not rejected by an injected fault.
func (s *Server) Mutations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutations
}

// FormField is one parsed multipart field.
type FormField struct {
	Name, Value string
	FileName    string // set for file parts
	FileSize    int64
}

// RecordedRequest is a request seen by the server.
type RecordedRequest struct {
	Method, Path string
	Query        string
	Header       http.Header
	Body         []byte
	Form         []FormField // multipart fields in order
	// ContentLength is the declared request Content-Length (-1 if unknown).
	ContentLength int64
	// BodyBytes is the number of body bytes actually received (multipart: set after parsing).
	BodyBytes int64
}

// Requests returns copies of all recorded requests in arrival order.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordedRequest, len(s.reqs))
	for i, r := range s.reqs {
		c := *r
		c.Form = append([]FormField(nil), r.Form...)
		out[i] = c
	}
	return out
}
