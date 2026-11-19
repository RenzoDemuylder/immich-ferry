//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"immich-ferry/test/e2e/testutil"
)

// TestSmokeAPIFacts verifies the "Verified API facts" of docs/DESIGN.md against the real servers.
// Contradictions fail with a message starting with "DESIGN CONTRADICTED". Findings that DESIGN does not
// pin are logged as "FINDING".
func TestSmokeAPIFacts(t *testing.T) {
	env := testutil.E2EEnv()
	for _, side := range []struct{ name, url string }{{"A", env["IMMICH_A_URL"]}, {"B", env["IMMICH_B_URL"]}} {
		side := side
		t.Run(side.name, func(t *testing.T) {
			if side.url == "" {
				t.Skip("IMMICH_*_URL not set; run `make e2e-up` first")
			}
			smokeInstance(t, side.url)
		})
	}
}

func contradicted(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Errorf("DESIGN CONTRADICTED: "+format, args...)
}

func finding(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("FINDING: "+format, args...)
}

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func hasID(as []testutil.Asset, id string) bool {
	for _, a := range as {
		if a.ID == id {
			return true
		}
	}
	return false
}

func smokeInstance(t *testing.T, url string) {
	ctx := context.Background()
	inst, err := testutil.Bootstrap(ctx, url)
	must(t, err, "bootstrap")
	admin := inst.AdminAPI()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	api, userID, err := inst.NewUserAPI(ctx, "smoke1-"+suffix+"@immich-ferry.test", "Smoke One", testutil.MinimalPermissions)
	must(t, err, "create user1")
	seed := time.Now().UnixNano()

	// --- version / identity ---
	var ver struct{ Major, Minor, Patch int }
	must(t, api.Do(ctx, http.MethodGet, "/server/version", nil, &ver), "server/version")
	t.Logf("server version %d.%d.%d", ver.Major, ver.Minor, ver.Patch)
	if ver.Major != 3 || ver.Minor < 2 {
		contradicted(t, "server version %+v, expected 3.>=2", ver)
	}
	var me struct {
		Permissions []string `json:"permissions"`
	}
	must(t, api.Do(ctx, http.MethodGet, "/api-keys/me", nil, &me), "api-keys/me")
	got := append([]string(nil), me.Permissions...)
	want := append([]string(nil), testutil.MinimalPermissions...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		contradicted(t, "/api-keys/me permissions = %v, want %v", got, want)
	}
	var um struct {
		ID string `json:"id"`
	}
	must(t, api.Do(ctx, http.MethodGet, "/users/me", nil, &um), "users/me with user.read")
	if um.ID != userID {
		contradicted(t, "/users/me id %q != login userId %q", um.ID, userID)
	}

	// --- upload / duplicate / naive datetime ---
	taken := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	lat, lon := 48.8584, 2.2945
	jpg := testutil.JPEG(testutil.JPEGOpts{Seed: seed, DateTimeOriginal: &taken, OffsetTime: "+02:00", Lat: &lat, Lon: &lon})
	created := time.Date(2021, 5, 6, 5, 8, 9, 0, time.UTC)
	id1, status, err := api.UploadStatus(ctx, "smoke-1.jpg", jpg, testutil.UploadOpts{FileCreatedAt: created})
	must(t, err, "upload jpeg")
	if status != 201 {
		contradicted(t, "first upload status %d, want 201", status)
	}
	id1b, status, err := api.UploadStatus(ctx, "smoke-1.jpg", jpg, testutil.UploadOpts{FileCreatedAt: created})
	must(t, err, "re-upload jpeg")
	if status != 200 || id1b != id1 {
		contradicted(t, "duplicate upload: status %d id %s (want 200 and %s)", status, id1b, id1)
	}
	_, status, err = api.UploadStatus(ctx, "naive.jpg", testutil.JPEG(testutil.JPEGOpts{Seed: seed + 1}), testutil.UploadOpts{FileCreatedRaw: "2021-05-06T07:08:09"})
	if status != 400 {
		contradicted(t, "upload with naive fileCreatedAt: status %d err %v, want 400", status, err)
	} else {
		t.Logf("naive datetime -> 400: %s", testutil.ErrMessage(err))
	}

	// --- checksum / extraction / EXIF ---
	testutil.WaitExtracted(t, api, id1)
	a1, err := api.GetAssetTyped(ctx, id1)
	must(t, err, "get asset")
	if a1.Checksum != testutil.SHA1Base64(jpg) {
		contradicted(t, "checksum %q != base64 sha1 of upload %q", a1.Checksum, testutil.SHA1Base64(jpg))
	}
	if a1.OriginalFileName != "smoke-1.jpg" {
		contradicted(t, "originalFileName %q", a1.OriginalFileName)
	}
	if a1.OwnerID != userID || a1.LibraryID != nil {
		contradicted(t, "ownerId %q libraryId %v", a1.OwnerID, a1.LibraryID)
	}
	if !a1.FileCreatedAt.Equal(created) {
		contradicted(t, "fileCreatedAt %s != uploaded %s", a1.FileCreatedAt, created)
	}
	if a1.Exif == nil || a1.Exif.DateTimeOriginal == nil || !a1.Exif.DateTimeOriginal.Equal(created) {
		contradicted(t, "extracted dateTimeOriginal %+v, want %s (UTC instant of 07:08:09+02:00)", a1.Exif, created)
	} else {
		tz := "<nil>"
		if a1.Exif.TimeZone != nil {
			tz = *a1.Exif.TimeZone
		}
		t.Logf("extracted exif: dateTimeOriginal=%s timeZone=%s lat=%v lon=%v", a1.Exif.DateTimeOriginal, tz, a1.Exif.Latitude, a1.Exif.Longitude)
		if a1.Exif.TimeZone == nil || *a1.Exif.TimeZone != "UTC+2" {
			finding(t, "extracted timeZone for OffsetTimeOriginal +02:00 is %s (expected UTC+2)", tz)
		}
	}
	if a1.Exif == nil || a1.Exif.Latitude == nil || a1.Exif.Longitude == nil ||
		abs(*a1.Exif.Latitude-lat) > 1e-4 || abs(*a1.Exif.Longitude-lon) > 1e-4 {
		contradicted(t, "extracted GPS %+v, want %v,%v", a1.Exif, lat, lon)
	}
	if a1.Visibility != "timeline" || a1.IsTrashed || a1.IsFavorite {
		contradicted(t, "fresh asset state: visibility=%s trashed=%v fav=%v", a1.Visibility, a1.IsTrashed, a1.IsFavorite)
	}
	_, orig, err := api.Raw(ctx, http.MethodGet, "/assets/"+id1+"/original", nil)
	must(t, err, "download original")
	if testutil.SHA1Base64(orig) != a1.Checksum {
		contradicted(t, "downloaded original sha1 != checksum (len %d vs %d)", len(orig), len(jpg))
	}

	// --- albums: create, add, duplicate, assetCount, updatedAt ---
	albumID, err := api.CreateAlbum(ctx, "smoke-album-"+suffix, "")
	must(t, err, "create album")
	al0, err := api.GetAlbum(ctx, albumID)
	must(t, err, "get album")
	if len(al0.Users) == 0 || al0.Users[0].User.ID != userID || al0.Users[0].Role != "owner" {
		finding(t, "new album albumUsers = %+v (DESIGN rule 12: owner first)", al0.Users)
	}
	id2, _, err := api.Upload(ctx, "smoke-2.jpg", testutil.JPEG(testutil.JPEGOpts{Seed: seed + 2}), testutil.UploadOpts{})
	must(t, err, "upload 2")
	time.Sleep(50 * time.Millisecond)
	res, err := api.AddToAlbum(ctx, albumID, id1, id2)
	must(t, err, "add to album")
	for _, r := range res {
		if !r.Success {
			contradicted(t, "album add %s failed: %s", r.ID, r.Error)
		}
	}
	al1, err := api.GetAlbum(ctx, albumID)
	must(t, err, "get album 1")
	if al1.AssetCount != 2 {
		contradicted(t, "assetCount %d, want 2", al1.AssetCount)
	}
	if !al1.UpdatedAt.After(al0.UpdatedAt) {
		contradicted(t, "album add did not bump updatedAt (%s -> %s)", al0.UpdatedAt, al1.UpdatedAt)
	}
	res, err = api.AddToAlbum(ctx, albumID, id1)
	must(t, err, "add dup (call itself)")
	if len(res) != 1 || res[0].Success || res[0].Error != "duplicate" {
		contradicted(t, "adding existing asset to album: %+v, want success=false error=duplicate", res)
	}
	time.Sleep(50 * time.Millisecond)
	res, err = api.RemoveFromAlbum(ctx, albumID, id2)
	must(t, err, "remove from album")
	if len(res) != 1 || !res[0].Success {
		contradicted(t, "album remove: %+v", res)
	}
	al2, err := api.GetAlbum(ctx, albumID)
	must(t, err, "get album 2")
	if !al2.UpdatedAt.Equal(al1.UpdatedAt) {
		contradicted(t, "album remove bumped updatedAt (%s -> %s); DESIGN says it does not", al1.UpdatedAt, al2.UpdatedAt)
	}
	if res, _ = api.RemoveFromAlbum(ctx, albumID, id2); len(res) == 1 {
		t.Logf("removing non-member: success=%v error=%q", res[0].Success, res[0].Error)
	}
	if al2.AssetCount != 1 {
		contradicted(t, "assetCount after remove %d, want 1", al2.AssetCount)
	}

	// --- album shared with me ---
	// second user for shared scenarios
	api2, user2ID, err := inst.NewUserAPI(ctx, "smoke2-"+suffix+"@immich-ferry.test", "Smoke Two", testutil.MinimalPermissions)
	must(t, err, "create user2")
	_, tok2, err := inst.CreateUser(ctx, "smoke2-"+suffix+"@immich-ferry.test", "user-password-1", "Smoke Two")
	must(t, err, "login user2")
	user2Bearer := &testutil.API{URL: url, Token: tok2}
	sharedName := "smoke-shared-" + suffix
	sharedID, err := user2Bearer.CreateAlbum(ctx, sharedName, "")
	must(t, err, "user2 create album")
	must(t, user2Bearer.ShareAlbum(ctx, sharedID, userID, "editor"), "user2 share album with user1")
	sh, err := api.GetAlbum(ctx, sharedID)
	if err != nil {
		contradicted(t, "GET /albums/{id} of album shared as editor: %v", err)
	} else {
		t.Logf("shared album users: %+v", sh.Users)
	}
	// GET /albums without filters (album discovery): owned albums and albums shared with me,
	// with my role in albumUsers (owner listed with role owner).
	listed, err := api.ListAlbums(ctx)
	must(t, err, "list albums")
	roleIn := func(id string) string {
		for _, al := range listed {
			if al.ID != id {
				continue
			}
			for _, u := range al.Users {
				if u.User.ID == userID {
					return u.Role
				}
			}
			return "listed-without-me"
		}
		return "not-listed"
	}
	if r := roleIn(albumID); r != "owner" {
		contradicted(t, "GET /albums: own album role %q, want owner", r)
	}
	if r := roleIn(sharedID); r != "editor" {
		contradicted(t, "GET /albums: album shared as editor has role %q, want editor", r)
	}
	if listed2, err := api2.ListAlbums(ctx); err == nil {
		for _, al := range listed2 {
			if al.ID == albumID {
				contradicted(t, "GET /albums lists another user's unshared album")
			}
		}
	}

	// --- album created already shared (discovery share) ---
	// GET /users (user.read) lists other users; POST /albums with albumUsers needs only
	// album.create (MinimalPermissions has no albumUser.create); the caller's own id is
	// ignored; an unknown user is 400 "Invalid user" and creates nothing.
	var users []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	must(t, api.Do(ctx, http.MethodGet, "/users", nil, &users), "GET /users")
	user2Listed := false
	for _, u := range users {
		user2Listed = user2Listed || (u.ID == user2ID && u.Email == "smoke2-"+suffix+"@immich-ferry.test")
	}
	if !user2Listed {
		contradicted(t, "GET /users does not list another user with email: %+v", users)
	}
	var sharedNew testutil.Album
	err = api.Do(ctx, http.MethodPost, "/albums", map[string]any{"albumName": "smoke-born-shared-" + suffix, "description": "",
		"albumUsers": []map[string]string{{"userId": user2ID, "role": "editor"}, {"userId": userID, "role": "viewer"}}}, &sharedNew)
	if err != nil {
		contradicted(t, "POST /albums with albumUsers using album.create only: %v", err)
	} else {
		roles := map[string]string{}
		for _, u := range sharedNew.Users {
			roles[u.User.ID] = u.Role
		}
		if len(roles) != 2 || roles[userID] != "owner" || roles[user2ID] != "editor" {
			contradicted(t, "album created with albumUsers has users %v, want owner + user2 editor (own id ignored)", roles)
		}
	}
	badName := "smoke-bad-share-" + suffix
	_, _, err = api.Raw(ctx, http.MethodPost, "/albums", map[string]any{"albumName": badName,
		"albumUsers": []map[string]string{{"userId": "00000000-0000-4000-8000-000000000000", "role": "editor"}}})
	if testutil.StatusOf(err) != 400 || !strings.Contains(testutil.ErrMessage(err), "Invalid user") {
		contradicted(t, "POST /albums with unknown user: %v, want 400 Invalid user", err)
	}
	if l, err := api.ListAlbums(ctx); err == nil {
		for _, al := range l {
			if al.Name == badName {
				contradicted(t, "POST /albums with unknown user created the album anyway")
			}
		}
	}

	// --- GET album with random uuid ---
	_, err = api.GetAlbum(ctx, "00000000-0000-4000-8000-000000000000")
	if testutil.StatusOf(err) != 400 || !strings.Contains(testutil.ErrMessage(err), "Not found") {
		contradicted(t, "GET unknown album: %v, want 400 'Not found or no album.read access'", err)
	} else {
		t.Logf("unknown album -> %v", err)
	}

	// --- structured search listing / trash / probe / duplicate-trashed / restore ---
	list, err := api.AlbumAssets(ctx, albumID)
	must(t, err, "album listing")
	if len(list) != 1 || list[0].ID != id1 || list[0].Exif == nil {
		contradicted(t, "structured listing = %d items (want [%s] with exifInfo)", len(list), id1)
	}
	must(t, api.Trash(ctx, id1), "trash")
	list, err = api.AlbumAssets(ctx, albumID)
	must(t, err, "album listing after trash")
	if len(list) != 0 {
		contradicted(t, "structured listing still contains trashed asset")
	}
	if al, _ := api.GetAlbum(ctx, albumID); al != nil && al.AssetCount != 0 {
		contradicted(t, "assetCount %d after trashing the only asset, want 0", al.AssetCount)
	}
	probe, err := api.SearchStructured(ctx, map[string]any{"filter": map[string]any{
		"albumIds": map[string]any{"any": []string{albumID}},
		"checksum": map[string]any{"in": []string{a1.Checksum}},
	}})
	must(t, err, "probe")
	if len(probe) != 1 || probe[0].ID != id1 || !probe[0].IsTrashed {
		contradicted(t, "probe (albumIds+checksum.in, no trash filter) = %+v, want trashed asset %s", probe, id1)
	}
	dupID, status, err := api.UploadStatus(ctx, "smoke-1.jpg", jpg, testutil.UploadOpts{FileCreatedAt: created})
	if err != nil || status != 200 || dupID != id1 {
		contradicted(t, "re-upload of trashed bytes: id %s status %d err %v, want 200 duplicate %s", dupID, status, err, id1)
	}
	var chk struct {
		Results []struct {
			ID        string `json:"id"`
			Action    string `json:"action"`
			Reason    string `json:"reason"`
			AssetID   string `json:"assetId"`
			IsTrashed bool   `json:"isTrashed"`
		} `json:"results"`
	}
	must(t, api.Do(ctx, http.MethodPost, "/assets/bulk-upload-check", map[string]any{"assets": []map[string]string{{"id": "x", "checksum": a1.Checksum}}}, &chk), "bulk-upload-check")
	if len(chk.Results) != 1 || chk.Results[0].Action != "reject" || chk.Results[0].AssetID != id1 || !chk.Results[0].IsTrashed {
		contradicted(t, "bulk-upload-check of trashed dup = %+v, want reject/assetId=%s/isTrashed=true", chk.Results, id1)
	}
	must(t, api.Restore(ctx, id1), "restore")
	ra, err := api.GetAssetTyped(ctx, id1)
	must(t, err, "get after restore")
	if ra.IsTrashed {
		contradicted(t, "asset still trashed after POST /trash/restore/assets")
	}
	if list, _ = api.AlbumAssets(ctx, albumID); !hasID(list, id1) {
		contradicted(t, "restored asset not back in album listing")
	}

	// --- legacy flat withExif vs structured, right after upload (before extraction) ---
	idRace, _, err := api.Upload(ctx, "smoke-race.jpg", testutil.JPEG(testutil.JPEGOpts{Seed: seed + 3}), testutil.UploadOpts{})
	must(t, err, "upload race")
	_, err = api.AddToAlbum(ctx, albumID, idRace)
	must(t, err, "add race")
	var flat struct {
		Assets struct {
			Items []testutil.Asset `json:"items"`
		} `json:"assets"`
	}
	if err := api.Do(ctx, http.MethodPost, "/search/metadata", map[string]any{"albumIds": []string{albumID}, "withExif": true}, &flat); err != nil {
		finding(t, "legacy flat search with albumIds+withExif failed: %v", err)
	} else {
		structured, _ := api.AlbumAssets(ctx, albumID)
		finding(t, "right after upload+add: legacy flat withExif returned %d items, structured returned %d", len(flat.Assets.Items), len(structured))
		if !hasID(structured, idRace) {
			contradicted(t, "structured listing dropped a not-yet-extracted asset")
		}
	}
	if err := api.Do(ctx, http.MethodPost, "/search/metadata", map[string]any{"albumIds": []string{albumID}, "filter": map[string]any{"albumIds": map[string]any{"any": []string{albumID}}}}, nil); testutil.StatusOf(err) != 400 {
		contradicted(t, "mixing flat and structured search fields: %v, want 400", err)
	}

	// --- PUT /assets ---
	testutil.WaitExtracted(t, api, id1)
	upd, err := api.UpdateAsset(ctx, id1, map[string]any{"dateTimeOriginal": "2022-07-08T09:10:11+02:00", "description": "smoke desc"})
	must(t, err, "PUT dateTimeOriginal+description")
	if upd.Exif == nil || upd.Exif.TimeZone == nil || *upd.Exif.TimeZone != "UTC+2" {
		contradicted(t, "PUT dateTimeOriginal +02:00 -> timeZone %+v, want UTC+2", upd.Exif)
	}
	if upd.Exif == nil || upd.Exif.DateTimeOriginal == nil || !upd.Exif.DateTimeOriginal.Equal(time.Date(2022, 7, 8, 7, 10, 11, 0, time.UTC)) {
		contradicted(t, "PUT dateTimeOriginal read-back %+v", upd.Exif)
	}
	if upd.Exif == nil || upd.Exif.Description == nil || *upd.Exif.Description != "smoke desc" {
		contradicted(t, "PUT description read-back %+v", upd.Exif)
	}
	if _, err = api.UpdateAsset(ctx, id1, map[string]any{"rating": 0}); testutil.StatusOf(err) != 400 {
		contradicted(t, "PUT rating 0: %v, want 400", err)
	}
	upd, err = api.UpdateAsset(ctx, id1, map[string]any{"rating": 3, "isFavorite": true, "latitude": 10.5, "longitude": -20.25})
	must(t, err, "PUT rating/fav/location")
	if upd.Exif == nil || upd.Exif.Rating == nil || *upd.Exif.Rating != 3 || !upd.IsFavorite ||
		upd.Exif.Latitude == nil || abs(*upd.Exif.Latitude-10.5) > 1e-6 {
		contradicted(t, "PUT rating/favorite/location read-back %+v fav=%v", upd.Exif, upd.IsFavorite)
	}
	// Does the rating-0 / unset rating ever appear?
	if raw, _ := api.GetAsset(ctx, id2); raw != nil {
		if ex, _ := raw["exifInfo"].(map[string]any); ex != nil {
			t.Logf("fresh asset exifInfo.rating = %v", ex["rating"])
			if r, ok := ex["rating"].(float64); ok && r == 0 {
				contradicted(t, "rating 0 appears in exifInfo")
			}
		}
	}

	// --- live photo ---
	mov := testutil.MOV(seed + 10)
	motionID, status, err := api.UploadStatus(ctx, "live-1.mov", mov, testutil.UploadOpts{Visibility: "hidden"})
	must(t, err, "upload motion hidden")
	if status != 201 {
		contradicted(t, "motion upload status %d", status)
	}
	stillID, _, err := api.Upload(ctx, "live-1.jpg", testutil.JPEG(testutil.JPEGOpts{Seed: seed + 11}), testutil.UploadOpts{LivePhotoVideoID: motionID})
	must(t, err, "upload still with livePhotoVideoId")
	still, err := api.GetAssetTyped(ctx, stillID)
	must(t, err, "get still")
	motion, err := api.GetAssetTyped(ctx, motionID)
	must(t, err, "get motion")
	if still.LivePhotoVideoID == nil || *still.LivePhotoVideoID != motionID {
		contradicted(t, "still.livePhotoVideoId = %v, want %s", still.LivePhotoVideoID, motionID)
	}
	if motion.Visibility != "hidden" {
		contradicted(t, "motion visibility %s, want hidden", motion.Visibility)
	}
	res, err = api.AddToAlbum(ctx, albumID, motionID)
	must(t, err, "add hidden motion to album")
	if len(res) != 1 || !res[0].Success {
		contradicted(t, "album add of hidden asset: %+v (DESIGN: no visibility check)", res)
	}
	if list, _ = api.AlbumAssets(ctx, albumID); hasID(list, motionID) {
		contradicted(t, "structured listing returned hidden motion")
	}
	probe, err = api.SearchStructured(ctx, map[string]any{"filter": map[string]any{
		"albumIds": map[string]any{"any": []string{albumID}},
		"checksum": map[string]any{"in": []string{motion.Checksum}},
	}})
	must(t, err, "probe hidden")
	if !hasID(probe, motionID) {
		contradicted(t, "probe did not return hidden album member")
	}
	if al, _ := api.GetAlbum(ctx, albumID); al != nil {
		list, _ = api.AlbumAssets(ctx, albumID)
		if al.AssetCount != len(list) {
			contradicted(t, "assetCount %d with a hidden member != visible listing %d", al.AssetCount, len(list))
		}
	}
	// PUT link: motion in timeline -> hidden, album membership kept.
	motion2ID, _, err := api.Upload(ctx, "live-2.mov", testutil.MOV(seed+12), testutil.UploadOpts{})
	must(t, err, "upload motion2")
	still2ID, _, err := api.Upload(ctx, "live-2.jpg", testutil.JPEG(testutil.JPEGOpts{Seed: seed + 13}), testutil.UploadOpts{})
	must(t, err, "upload still2")
	if _, err := api.AddToAlbum(ctx, albumID, motion2ID); err != nil {
		must(t, err, "add motion2")
	}
	if _, err = api.UpdateAsset(ctx, still2ID, map[string]any{"livePhotoVideoId": motion2ID}); err != nil {
		contradicted(t, "PUT livePhotoVideoId: %v", err)
	} else {
		m2, _ := api.GetAssetTyped(ctx, motion2ID)
		if m2 == nil || m2.Visibility != "hidden" {
			contradicted(t, "PUT livePhotoVideoId did not hide motion: %+v", m2)
		}
		probe, _ = api.SearchStructured(ctx, map[string]any{"filter": map[string]any{
			"albumIds": map[string]any{"any": []string{albumID}},
			"checksum": map[string]any{"in": []string{testutilChecksum(m2)}},
		}})
		if !hasID(probe, motion2ID) {
			contradicted(t, "PUT livePhotoVideoId removed motion from album (DESIGN: does not)")
		}
	}

	// --- video fixtures valid ---
	mp4ID, status, err := api.UploadStatus(ctx, "smoke.mp4", testutil.MP4(seed+20), testutil.UploadOpts{})
	if err != nil || status != 201 {
		contradicted(t, "MP4 fixture upload: status %d err %v", status, err)
	} else {
		testutil.WaitExtracted(t, api, mp4ID)
		if v, _ := api.GetAssetTyped(ctx, mp4ID); v != nil {
			t.Logf("mp4: type=%s duration=%v exif=%+v", v.Type, v.Duration, v.Exif)
			if v.Type != "VIDEO" {
				contradicted(t, "mp4 asset type %s", v.Type)
			}
		}
	}

	// --- two owners in one album: listing covers all owners; editor/owner removal permissions ---
	own2ID, _, err := api2.Upload(ctx, "smoke-user2.jpg", testutil.JPEG(testutil.JPEGOpts{Seed: seed + 30}), testutil.UploadOpts{})
	must(t, err, "user2 upload")
	albumOfU2, err := user2Bearer.CreateAlbum(ctx, "smoke-u2-owned-"+suffix, "", own2ID)
	must(t, err, "user2 album")
	must(t, user2Bearer.ShareAlbum(ctx, albumOfU2, userID, "editor"), "share u2 album to user1")
	u1Own, _, err := api.Upload(ctx, "smoke-user1-in-u2.jpg", testutil.JPEG(testutil.JPEGOpts{Seed: seed + 31}), testutil.UploadOpts{})
	must(t, err, "user1 upload")
	res, err = api.AddToAlbum(ctx, albumOfU2, u1Own)
	must(t, err, "editor add to others' album")
	if len(res) != 1 || !res[0].Success {
		contradicted(t, "editor add into shared album: %+v", res)
	}
	list, err = api.AlbumAssets(ctx, albumOfU2)
	must(t, err, "listing shared album as editor")
	if !hasID(list, own2ID) || !hasID(list, u1Own) {
		contradicted(t, "editor listing of shared album lacks other owner's asset: have %d items", len(list))
	}
	res, err = api.RemoveFromAlbum(ctx, albumOfU2, own2ID)
	must(t, err, "editor remove other's asset (call)")
	if len(res) == 1 && res[0].Success {
		// Real v3.4.1 behaviour: an editor may remove ANY asset (even another owner's) from the album.
		// DESIGN Appendix B rule 4 (fakeimmich: editor -> no_permission) is therefore wrong for v3.4.1.
		finding(t, "CONTRADICTS Appendix B rule 4: editor removing another owner's asset SUCCEEDED: %+v", res)
		if list, _ = api.AlbumAssets(ctx, albumOfU2); hasID(list, own2ID) {
			contradicted(t, "editor remove reported success but asset still listed")
		}
	} else {
		finding(t, "editor removing another owner's asset -> %+v", res)
		if len(res) != 1 || res[0].Error != "no_permission" {
			contradicted(t, "editor remove of other's asset: %+v, want success or no_permission", res)
		}
	}
	// owner (user2, minimal key) removes editor's asset
	if res, err = api2.RemoveFromAlbum(ctx, albumOfU2, u1Own); err != nil || len(res) != 1 || !res[0].Success {
		contradicted(t, "album owner removing another user's asset: %+v err %v", res, err)
	}
	_ = user2ID

	// --- queues ---
	testutil.WaitQueuesIdle(t, admin)
}

func testutilChecksum(a *testutil.Asset) string {
	if a == nil {
		return ""
	}
	return a.Checksum
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
