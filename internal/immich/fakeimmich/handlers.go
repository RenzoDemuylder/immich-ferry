package fakeimmich

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"immich-ferry/internal/immich"
)

const recKey ctxKey = 2

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

type fieldErr struct{ Path, Message string }

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, msg string, details ...fieldErr) {
	cid := newUUID()[:8]
	w.Header().Set("X-Correlation-ID", cid)
	body := map[string]any{"message": msg, "statusCode": status, "error": http.StatusText(status), "correlationId": cid}
	if len(details) > 0 {
		var errs []map[string]any
		for _, d := range details {
			errs = append(errs, map[string]any{"path": []string{d.Path}, "message": d.Message})
		}
		body["errors"] = errs
	}
	writeJSON(w, status, body)
}

func badRequest(w http.ResponseWriter, path, msg string) {
	writeErr(w, 400, "Validation failed", fieldErr{path, msg})
}

func noAccess(w http.ResponseWriter, perm string) {
	writeErr(w, 400, "Not found or no "+perm+" access")
}

func fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

type authedHandler func(w http.ResponseWriter, r *http.Request, u *user)

// auth authenticates the API key and checks the permission ("" = none needed).
func (s *Server) auth(perm string, h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		u := s.keys[r.Header.Get("x-api-key")]
		s.mu.Unlock()
		if u == nil {
			writeErr(w, 401, "Invalid API key")
			return
		}
		if perm != "" {
			ok := false
			for _, p := range u.perms {
				if p == "all" || p == perm {
					ok = true
				}
			}
			if !ok {
				writeErr(w, 403, "Missing required permission: "+perm)
				return
			}
		}
		h(w, r, u)
	}
}

func (s *Server) routes() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/server/version", s.hVersion)
	m.HandleFunc("GET /api/api-keys/me", s.auth("", s.hKeyMe))
	m.HandleFunc("GET /api/users/me", s.auth("user.read", s.hUserMe))
	m.HandleFunc("GET /api/users", s.auth("user.read", s.hUserList))
	m.HandleFunc("GET /api/albums", s.auth("album.read", s.hAlbumList))
	m.HandleFunc("POST /api/albums", s.auth("album.create", s.hAlbumCreate))
	m.HandleFunc("GET /api/albums/{id}", s.auth("album.read", s.hAlbumGet))
	m.HandleFunc("PATCH /api/albums/{id}", s.auth("album.update", s.hAlbumPatch))
	m.HandleFunc("DELETE /api/albums/{id}", s.auth("album.delete", s.hAlbumDelete))
	m.HandleFunc("DELETE /api/albums/{id}/user/{userId}", s.auth("albumUser.delete", s.hAlbumUserRemove))
	m.HandleFunc("DELETE /api/assets", s.auth("asset.delete", s.hAssetsDelete))
	m.HandleFunc("PUT /api/albums/{id}/assets", s.auth("albumAsset.create", s.hAlbumAdd))
	m.HandleFunc("DELETE /api/albums/{id}/assets", s.auth("albumAsset.delete", s.hAlbumRemove))
	m.HandleFunc("POST /api/search/metadata", s.auth("asset.read", s.hSearch))
	m.HandleFunc("POST /api/assets/bulk-upload-check", s.auth("asset.upload", s.hUploadCheck))
	m.HandleFunc("POST /api/assets", s.auth("asset.upload", s.hUpload))
	m.HandleFunc("GET /api/assets/{id}", s.auth("asset.read", s.hAssetGet))
	m.HandleFunc("PUT /api/assets/{id}", s.auth("asset.update", s.hAssetPut))
	m.HandleFunc("GET /api/assets/{id}/original", s.auth("asset.download", s.hDownload))
	m.HandleFunc("POST /api/trash/restore/assets", s.auth("asset.delete", s.hRestore))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 404, "Cannot "+r.Method+" "+r.URL.Path)
	})
	return m
}

// decodeBody decodes the JSON request body into v; on failure it writes a 400.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeErr(w, 400, "Validation failed", fieldErr{"body", "invalid JSON: " + err.Error()})
		return false
	}
	return true
}

// ---- access helpers (caller holds s.mu) ----

func (s *Server) albumRole(userID string, al *FakeAlbum) string {
	if al.OwnerID == userID {
		return "owner"
	}
	for _, u := range al.Users {
		if u.UserID == userID {
			return u.Role
		}
	}
	return ""
}

// canReadAsset: owner (non-locked), or member of an album containing it (non-trashed),
// or motion video of such a still. Locked assets are never accessible to API keys.
func (s *Server) canReadAsset(userID string, a *FakeAsset) bool {
	if a.Visibility == immich.VisibilityLocked {
		return false
	}
	if a.OwnerID == userID {
		return true
	}
	for _, al := range s.albums {
		if s.albumRole(userID, al) == "" {
			continue
		}
		for _, id := range al.AssetIDs {
			if id == a.ID && !a.IsTrashed {
				return true
			}
			o := s.assets[id]
			if o != nil && !o.IsTrashed && o.LivePhotoVideoID == a.ID && o.Visibility != immich.VisibilityLocked {
				return true
			}
		}
	}
	return false
}

// ---- serialization ----

func (s *Server) albumJSON(al *FakeAlbum) map[string]any {
	count := 0
	for _, id := range al.AssetIDs {
		a := s.assets[id]
		if a != nil && !a.IsTrashed && (a.Visibility == immich.VisibilityTimeline || a.Visibility == immich.VisibilityArchive) {
			count++
		}
	}
	userObj := func(id, role string) map[string]any {
		u := s.users[id]
		name, email := id, ""
		if u != nil {
			name, email = u.name, u.email
		}
		return map[string]any{"role": role, "user": map[string]any{"id": id, "name": name, "email": email}}
	}
	users := []any{userObj(al.OwnerID, "owner")}
	for _, u := range al.Users {
		users = append(users, userObj(u.UserID, u.Role))
	}
	return map[string]any{
		"id": al.ID, "albumName": al.Name, "description": al.Description, "assetCount": count,
		"createdAt": fmtTime(al.CreatedAt), "updatedAt": fmtTime(al.UpdatedAt),
		"albumUsers": users, "shared": len(al.Users) > 0, "hasSharedLink": false,
		"isActivityEnabled": true, "albumThumbnailAssetId": nil, "order": "desc",
	}
}

func exifJSON(e *immich.Exif) map[string]any {
	m := map[string]any{"description": nil, "dateTimeOriginal": nil, "timeZone": nil, "latitude": nil,
		"longitude": nil, "rating": nil, "fileSizeInByte": nil}
	if e == nil {
		return m
	}
	if e.Description != nil {
		m["description"] = *e.Description
	}
	if e.DateTimeOriginal != nil {
		m["dateTimeOriginal"] = fmtTime(*e.DateTimeOriginal)
	}
	if e.TimeZone != nil {
		m["timeZone"] = *e.TimeZone
	}
	if e.Latitude != nil {
		m["latitude"] = *e.Latitude
	}
	if e.Longitude != nil {
		m["longitude"] = *e.Longitude
	}
	if e.Rating != nil {
		m["rating"] = *e.Rating
	}
	if e.FileSizeInByte != nil {
		m["fileSizeInByte"] = *e.FileSizeInByte
	}
	return m
}

// assetJSON renders an AssetResponseDto for caller. withExif forces exifInfo
// (left-join semantics: object with null fields when no exif).
func (s *Server) assetJSON(a *FakeAsset, caller string, withExif bool) map[string]any {
	m := map[string]any{
		"id": a.ID, "ownerId": a.OwnerID, "checksum": a.Checksum, "libraryId": nil,
		"originalFileName": a.OriginalFileName, "originalPath": "/data/upload/" + a.OwnerID + "/" + a.OriginalFileName,
		"originalMimeType": "application/octet-stream",
		"fileCreatedAt":    fmtTime(a.FileCreatedAt), "fileModifiedAt": fmtTime(a.FileModifiedAt),
		"localDateTime": fmtTime(a.FileCreatedAt), "createdAt": fmtTime(a.FileCreatedAt), "updatedAt": fmtTime(a.UpdatedAt),
		"type": a.Type, "duration": nil, "livePhotoVideoId": nil,
		"isFavorite": a.IsFavorite && a.OwnerID == caller, "isTrashed": a.IsTrashed, "isOffline": a.IsOffline,
		"isArchived": a.Visibility == immich.VisibilityArchive, "isEdited": false, "hasMetadata": a.Exif != nil,
		"visibility": string(a.Visibility), "thumbhash": nil, "width": nil, "height": nil,
	}
	if a.LibraryID != "" {
		m["libraryId"] = a.LibraryID
	}
	if a.DurationMS != nil {
		m["duration"] = *a.DurationMS
	}
	if a.LivePhotoVideoID != "" {
		m["livePhotoVideoId"] = a.LivePhotoVideoID
	}
	if a.Exif != nil || withExif {
		m["exifInfo"] = exifJSON(a.Exif)
	}
	return m
}

// ---- simple endpoints ----

func (s *Server) hVersion(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	v := s.version
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"major": v[0], "minor": v[1], "patch": v[2], "prerelease": nil, "featureLevel": 1})
}

func (s *Server) hKeyMe(w http.ResponseWriter, r *http.Request, u *user) {
	writeJSON(w, 200, map[string]any{"id": newUUID(), "name": "fake key", "permissions": u.perms,
		"createdAt": fmtTime(time.Now()), "updatedAt": fmtTime(time.Now())})
}

func (s *Server) hUserMe(w http.ResponseWriter, r *http.Request, u *user) {
	writeJSON(w, 200, map[string]any{"id": u.id, "name": u.name, "email": u.email})
}

// hUserList serves GET /users: every user (default "public users" server setting).
func (s *Server) hUserList(w http.ResponseWriter, r *http.Request, u *user) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []any{}
	for _, x := range s.users {
		out = append(out, map[string]any{"id": x.id, "name": x.name, "email": x.email})
	}
	writeJSON(w, 200, out)
}

// ---- albums ----

// hAlbumList serves GET /albums: owned and shared-with-me albums (any role), newest first.
func (s *Server) hAlbumList(w http.ResponseWriter, r *http.Request, u *user) {
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []*FakeAlbum
	for _, id := range s.albumSeq {
		al := s.albums[id]
		role := s.albumRole(u.id, al)
		if role == "" {
			continue
		}
		if v := q.Get("isOwned"); v != "" && (v == "true") != (role == "owner") {
			continue
		}
		if v := q.Get("isShared"); v != "" && (v == "true") != (len(al.Users) > 0) {
			continue
		}
		// ?assetId: only the caller's albums (owned or shared) holding the asset (verified v3.4.1).
		if v := q.Get("assetId"); v != "" && !slices.Contains(al.AssetIDs, v) {
			continue
		}
		list = append(list, al)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	out := []any{}
	for _, al := range list {
		out = append(out, s.albumJSON(al))
	}
	writeJSON(w, 200, out)
}

func (s *Server) hAlbumCreate(w http.ResponseWriter, r *http.Request, u *user) {
	var body struct {
		AlbumName   *string  `json:"albumName"`
		Description *string  `json:"description"`
		AssetIDs    []string `json:"assetIds"`
		AlbumUsers  []struct {
			UserID string `json:"userId"`
			Role   string `json:"role"`
		} `json:"albumUsers"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.AlbumName == nil {
		badRequest(w, "albumName", "Invalid input: expected string, received undefined")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	desc := ""
	if body.Description != nil {
		desc = *body.Description
	}
	// v3.4.1 (verified): unknown user → 400 "Invalid user"; the caller's own id is
	// ignored; a role other than editor|viewer or a repeated user → 500. Nothing is
	// created on error.
	var share []immich.AlbumUser
	seen := map[string]bool{}
	for _, au := range body.AlbumUsers {
		if s.users[au.UserID] == nil {
			writeErr(w, 400, "Invalid user")
			return
		}
		if au.UserID == u.id {
			continue
		}
		if (au.Role != "editor" && au.Role != "viewer") || seen[au.UserID] {
			writeErr(w, 500, "Failed to create album")
			return
		}
		seen[au.UserID] = true
		share = append(share, immich.AlbumUser{UserID: au.UserID, Role: au.Role})
	}
	al := s.createAlbumLocked(u.id, *body.AlbumName, desc)
	al.Users = share
	for _, id := range body.AssetIDs {
		if a := s.assets[id]; a != nil && s.canReadAsset(u.id, a) {
			s.addToAlbumLocked(al, id)
		}
	}
	writeJSON(w, 201, s.albumJSON(al))
}

func (s *Server) hAlbumGet(w http.ResponseWriter, r *http.Request, u *user) {
	s.mu.Lock()
	defer s.mu.Unlock()
	al := s.albums[r.PathValue("id")]
	if al == nil || s.albumRole(u.id, al) == "" {
		noAccess(w, "album.read")
		return
	}
	writeJSON(w, 200, s.albumJSON(al))
}

func (s *Server) hAlbumPatch(w http.ResponseWriter, r *http.Request, u *user) {
	var raw map[string]json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	al := s.albums[r.PathValue("id")]
	if al == nil {
		noAccess(w, "album.update")
		return
	}
	if role := s.albumRole(u.id, al); role != "owner" && role != "editor" {
		noAccess(w, "album.update")
		return
	}
	if v, ok := raw["albumName"]; ok {
		var name string
		if json.Unmarshal(v, &name) != nil {
			badRequest(w, "albumName", "Invalid input: expected string")
			return
		}
		al.Name = name
	}
	if v, ok := raw["description"]; ok {
		var d *string
		if json.Unmarshal(v, &d) != nil {
			badRequest(w, "description", "Invalid input: expected string")
			return
		}
		al.Description = ""
		if d != nil {
			al.Description = *d
		}
	}
	al.UpdatedAt = s.tick()
	writeJSON(w, 200, s.albumJSON(al))
}

// hAlbumDelete serves DELETE /albums/{id}: owner only; the assets are kept (verified v3.4.1).
func (s *Server) hAlbumDelete(w http.ResponseWriter, r *http.Request, u *user) {
	s.mu.Lock()
	al := s.albums[r.PathValue("id")]
	if al == nil || s.albumRole(u.id, al) != "owner" {
		s.mu.Unlock()
		noAccess(w, "album.delete")
		return
	}
	s.mu.Unlock()
	s.DeleteAlbum(al.ID)
	w.WriteHeader(http.StatusNoContent)
}

// hAlbumUserRemove serves DELETE /albums/{id}/user/{userId} ("me" = caller). Verified
// v3.4.1: removing the owner is 400 "Cannot remove the last album owner"; a user who is
// not a member is 400 "Album not shared with user"; members may remove only themselves.
func (s *Server) hAlbumUserRemove(w http.ResponseWriter, r *http.Request, u *user) {
	s.mu.Lock()
	defer s.mu.Unlock()
	al := s.albums[r.PathValue("id")]
	if al == nil || s.albumRole(u.id, al) == "" {
		noAccess(w, "albumUser.delete")
		return
	}
	target := r.PathValue("userId")
	if target == "me" {
		target = u.id
	}
	if target == al.OwnerID {
		writeErr(w, 400, "Cannot remove the last album owner")
		return
	}
	if target != u.id && s.albumRole(u.id, al) != "owner" {
		noAccess(w, "albumUser.delete")
		return
	}
	for i, x := range al.Users {
		if x.UserID == target {
			al.Users = append(al.Users[:i:i], al.Users[i+1:]...)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeErr(w, 400, "Album not shared with user")
}

// hAssetsDelete serves DELETE /assets {ids, force}: own assets only; force=false trashes.
func (s *Server) hAssetsDelete(w http.ResponseWriter, r *http.Request, u *user) {
	var body struct {
		IDs   []string `json:"ids"`
		Force *bool    `json:"force"`
	}
	if !decodeBody(w, r, &body) || !validateIDs(w, body.IDs) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range body.IDs {
		if a := s.assets[id]; a == nil || a.OwnerID != u.id {
			noAccess(w, "asset.delete")
			return
		}
	}
	for _, id := range body.IDs {
		a := s.assets[id]
		if body.Force != nil && *body.Force {
			s.purgeLocked(id)
			continue
		}
		a.IsTrashed = true
		a.UpdatedAt = s.tick()
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateIDs(w http.ResponseWriter, ids []string) bool {
	if ids == nil {
		badRequest(w, "ids", "Invalid input: expected array")
		return false
	}
	for _, id := range ids {
		if !uuidRe.MatchString(id) {
			badRequest(w, "ids", "Invalid UUID: "+id)
			return false
		}
	}
	return true
}

func (s *Server) hAlbumAdd(w http.ResponseWriter, r *http.Request, u *user) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeBody(w, r, &body) || !validateIDs(w, body.IDs) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	al := s.albums[r.PathValue("id")]
	if al == nil {
		noAccess(w, "albumAsset.create")
		return
	}
	role := s.albumRole(u.id, al)
	if role == "" {
		noAccess(w, "albumAsset.create")
		return
	}
	res := []any{}
	for _, id := range body.IDs {
		a := s.assets[id]
		switch {
		case role == "viewer":
			res = append(res, map[string]any{"id": id, "success": false, "error": "no_permission"})
		case a == nil:
			res = append(res, map[string]any{"id": id, "success": false, "error": "not_found"})
		case !s.canReadAsset(u.id, a):
			res = append(res, map[string]any{"id": id, "success": false, "error": "no_permission"})
		case !s.addToAlbumLocked(al, id): // note: no visibility check (rule 3)
			res = append(res, map[string]any{"id": id, "success": false, "error": "duplicate"})
		default:
			res = append(res, map[string]any{"id": id, "success": true})
		}
	}
	writeJSON(w, 200, res)
}

func (s *Server) hAlbumRemove(w http.ResponseWriter, r *http.Request, u *user) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeBody(w, r, &body) || !validateIDs(w, body.IDs) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	al := s.albums[r.PathValue("id")]
	if al == nil {
		noAccess(w, "albumAsset.delete")
		return
	}
	role := s.albumRole(u.id, al)
	if role == "" {
		noAccess(w, "albumAsset.delete")
		return
	}
	res := []any{}
	for _, id := range body.IDs {
		in := false
		for _, x := range al.AssetIDs {
			if x == id {
				in = true
			}
		}
		switch {
		case !in:
			res = append(res, map[string]any{"id": id, "success": false, "error": "not_found"})
		case role == "viewer": // editors may remove any member asset (verified on real v3.4.1)
			res = append(res, map[string]any{"id": id, "success": false, "error": "no_permission"})
		default:
			s.removeFromAlbumLocked(al, id) // does not bump updatedAt (rule 3)
			res = append(res, map[string]any{"id": id, "success": true})
		}
	}
	writeJSON(w, 200, res)
}

// ---- assets ----

func (s *Server) hAssetGet(w http.ResponseWriter, r *http.Request, u *user) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.assets[r.PathValue("id")]
	if a == nil || !s.canReadAsset(u.id, a) {
		noAccess(w, "asset.read")
		return
	}
	writeJSON(w, 200, s.assetJSON(a, u.id, false))
}

func tzString(off int) string {
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	h, m := off/3600, (off%3600)/60
	if m == 0 {
		return fmt.Sprintf("UTC%s%d", sign, h)
	}
	return fmt.Sprintf("UTC%s%d:%02d", sign, h, m)
}

func (s *Server) hAssetPut(w http.ResponseWriter, r *http.Request, u *user) {
	var raw map[string]json.RawMessage
	if !decodeBody(w, r, &raw) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.assets[r.PathValue("id")]
	if a == nil || a.OwnerID != u.id || a.Visibility == immich.VisibilityLocked {
		noAccess(w, "asset.update")
		return
	}
	isNull := func(m json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(m), []byte("null")) }
	var (
		descr   *string
		dto     *time.Time
		dtoOff  int
		lat     *float64
		lon     *float64
		rating  **int
		fav     *bool
		vis     *immich.Visibility
		liveSet bool
		liveID  string
	)
	for k, v := range raw {
		if isNull(v) && k != "rating" && k != "livePhotoVideoId" {
			badRequest(w, k, "Invalid input: expected non-null value")
			return
		}
		switch k {
		case "description":
			var d string
			if json.Unmarshal(v, &d) != nil {
				badRequest(w, k, "Invalid input: expected string")
				return
			}
			descr = &d
		case "dateTimeOriginal":
			var str string
			if json.Unmarshal(v, &str) != nil {
				badRequest(w, k, "Invalid input: expected string")
				return
			}
			t, err := time.Parse(time.RFC3339Nano, str)
			if err != nil {
				badRequest(w, k, "Invalid ISO datetime (offset required)")
				return
			}
			_, off := t.Zone()
			dto, dtoOff = &t, off
		case "latitude", "longitude":
			var f float64
			if json.Unmarshal(v, &f) != nil {
				badRequest(w, k, "Invalid input: expected number")
				return
			}
			if (k == "latitude" && (f < -90 || f > 90)) || (k == "longitude" && (f < -180 || f > 180)) {
				badRequest(w, k, "Number out of range")
				return
			}
			if k == "latitude" {
				lat = &f
			} else {
				lon = &f
			}
		case "rating":
			var rp *int
			if json.Unmarshal(v, &rp) != nil || (rp != nil && *rp != -1 && (*rp < 1 || *rp > 5)) {
				badRequest(w, k, "Rating must be -1, 1-5 or null")
				return
			}
			rating = &rp
		case "isFavorite":
			var b bool
			if json.Unmarshal(v, &b) != nil {
				badRequest(w, k, "Invalid input: expected boolean")
				return
			}
			fav = &b
		case "visibility":
			var str string
			if json.Unmarshal(v, &str) != nil || !validVisibility(str) {
				badRequest(w, k, "Invalid option")
				return
			}
			vv := immich.Visibility(str)
			vis = &vv
		case "livePhotoVideoId":
			liveSet = true
			if !isNull(v) {
				if json.Unmarshal(v, &liveID) != nil || !uuidRe.MatchString(liveID) {
					badRequest(w, k, "Invalid UUID")
					return
				}
			}
		}
	}
	if (lat == nil) != (lon == nil) {
		badRequest(w, "latitude", "Latitude and longitude must be provided together")
		return
	}
	var motion *FakeAsset
	if liveSet && liveID != "" {
		motion = s.assets[liveID]
		if motion == nil || motion.OwnerID != u.id || motion.Type != "VIDEO" {
			writeErr(w, 400, "Live photo video must be an existing video you own")
			return
		}
	}
	ensureExif := func() {
		if a.Exif == nil {
			a.Exif = &immich.Exif{}
		}
	}
	if descr != nil {
		ensureExif()
		a.Exif.Description = descr
	}
	if dto != nil {
		ensureExif()
		utc := dto.UTC()
		tz := tzString(dtoOff)
		a.Exif.DateTimeOriginal, a.Exif.TimeZone = &utc, &tz
	}
	if lat != nil {
		ensureExif()
		a.Exif.Latitude, a.Exif.Longitude = lat, lon
	}
	if rating != nil {
		ensureExif()
		a.Exif.Rating = *rating
	}
	if fav != nil {
		a.IsFavorite = *fav
	}
	if liveSet {
		if motion == nil {
			a.LivePhotoVideoID = ""
		} else {
			a.LivePhotoVideoID = motion.ID
			if motion.Visibility == immich.VisibilityTimeline { // does NOT remove from albums
				motion.Visibility = immich.VisibilityHidden
				motion.UpdatedAt = s.tick()
			}
		}
	}
	a.UpdatedAt = s.tick()
	if vis != nil {
		s.setVisibilityLocked(a.ID, *vis)
	}
	writeJSON(w, 200, s.assetJSON(a, u.id, false))
}

func validVisibility(v string) bool {
	switch immich.Visibility(v) {
	case immich.VisibilityTimeline, immich.VisibilityArchive, immich.VisibilityHidden, immich.VisibilityLocked:
		return true
	}
	return false
}

func (s *Server) hDownload(w http.ResponseWriter, r *http.Request, u *user) {
	s.mu.Lock()
	a := s.assets[r.PathValue("id")]
	if a == nil || !s.canReadAsset(u.id, a) {
		s.mu.Unlock()
		noAccess(w, "asset.download")
		return
	}
	content := a.Content
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(content)))
	w.WriteHeader(200)
	limit := int64(len(content))
	truncate := false
	if f := faultFrom(r.Context()); f != nil && f.TruncateAt > 0 && f.TruncateAt < limit {
		limit, truncate = f.TruncateAt, true
	}
	fl, _ := w.(http.Flusher)
	const chunk = 32 << 10
	for off := int64(0); off < limit; {
		n := min(int64(chunk), limit-off)
		if _, err := w.Write(content[off : off+n]); err != nil {
			return
		}
		off += n
		if fl != nil {
			fl.Flush()
		}
	}
	if truncate {
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) hRestore(w http.ResponseWriter, r *http.Request, u *user) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeBody(w, r, &body) || !validateIDs(w, body.IDs) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range body.IDs {
		if a := s.assets[id]; a == nil || a.OwnerID != u.id {
			noAccess(w, "asset.delete")
			return
		}
	}
	n := 0
	for _, id := range body.IDs {
		if a := s.assets[id]; a.IsTrashed {
			a.IsTrashed = false
			a.UpdatedAt = s.tick()
			n++
		}
	}
	writeJSON(w, 200, map[string]any{"count": n})
}

func (s *Server) hUploadCheck(w http.ResponseWriter, r *http.Request, u *user) {
	var body struct {
		Assets []struct {
			ID       *string `json:"id"`
			Checksum *string `json:"checksum"`
		} `json:"assets"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Assets == nil {
		badRequest(w, "assets", "Invalid input: expected array")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	results := []any{}
	for _, it := range body.Assets {
		if it.ID == nil || it.Checksum == nil {
			badRequest(w, "assets", "id and checksum are required")
			return
		}
		var hit *FakeAsset
		for _, id := range s.order {
			a := s.assets[id]
			if a.OwnerID == u.id && a.Checksum == *it.Checksum && (hit == nil || (hit.IsTrashed && !a.IsTrashed)) {
				hit = a
			}
		}
		if hit == nil {
			results = append(results, map[string]any{"id": *it.ID, "action": "accept"})
		} else {
			results = append(results, map[string]any{"id": *it.ID, "action": "reject", "reason": "duplicate",
				"assetId": hit.ID, "isTrashed": hit.IsTrashed})
		}
	}
	writeJSON(w, 200, map[string]any{"results": results})
}

var _ = url.PathEscape
var _ = strings.TrimSpace
