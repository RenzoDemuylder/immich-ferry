//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"immich-ferry/test/e2e/testutil"
)

// teardownPermissions is the key of the sync user when discovery shares albums: the
// minimal set plus what the teardown after "all shared users left" needs.
var teardownPermissions = append(append([]string(nil), testutil.MinimalPermissions...), "album.delete", "albumUser.delete")

// TestSmokeTeardownFacts verifies the API facts the discovery teardown relies on
// (docs/DESIGN.md "Verified API facts": leave, album delete, trash, ?assetId).
func TestSmokeTeardownFacts(t *testing.T) {
	env := testutil.E2EEnv()
	for _, side := range []struct{ name, url string }{{"A", env["IMMICH_A_URL"]}, {"B", env["IMMICH_B_URL"]}} {
		t.Run(side.name, func(t *testing.T) {
			if side.url == "" {
				t.Skip("IMMICH_*_URL not set; run `make e2e-up` first")
			}
			smokeTeardown(t, side.url)
		})
	}
}

func albumIDsOf(als []testutil.Album) map[string]bool {
	m := map[string]bool{}
	for _, a := range als {
		m[a.ID] = true
	}
	return m
}

func smokeTeardown(t *testing.T, url string) {
	ctx := context.Background()
	inst, err := testutil.Bootstrap(ctx, url)
	must(t, err, "bootstrap")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	sync, syncID, err := inst.NewUserAPI(ctx, "td-sync-"+suffix+"@immich-ferry.test", "Teardown Sync", teardownPermissions)
	must(t, err, "create sync user")
	member, err := testutil.NewUser(ctx, inst, "td-member")
	must(t, err, "create member")

	created := time.Date(2022, 1, 2, 3, 4, 5, 0, time.UTC)
	upload := func(name string) string {
		id, _, err := sync.Upload(ctx, name, testutil.JPEG(testutil.JPEGOpts{Seed: testutil.UniqueSeed()}), testutil.UploadOpts{FileCreatedAt: created})
		must(t, err, "upload "+name)
		return id
	}
	a1, a2 := upload("td-1.jpg"), upload("td-2.jpg")

	// The sync user creates X' shared with the member (as discovery does) and an
	// unshared album Y that also holds a2.
	var x testutil.Album
	must(t, sync.Do(ctx, http.MethodPost, "/albums", map[string]any{"albumName": "td-x-" + suffix, "description": "",
		"albumUsers": []map[string]string{{"userId": member.ID, "role": "editor"}}}, &x), "create shared album")
	_, err = sync.AddToAlbum(ctx, x.ID, a1, a2)
	must(t, err, "add to X'")
	y, err := sync.CreateAlbum(ctx, "td-y-"+suffix, "", a2)
	must(t, err, "create Y")

	// GET /albums?assetId lists the caller's albums (owned or shared) holding the asset.
	var in1, in2 []testutil.Album
	must(t, sync.Do(ctx, http.MethodGet, "/albums?assetId="+a1, nil, &in1), "albums of a1")
	must(t, sync.Do(ctx, http.MethodGet, "/albums?assetId="+a2, nil, &in2), "albums of a2")
	if m := albumIDsOf(in1); len(m) != 1 || !m[x.ID] {
		contradicted(t, "GET /albums?assetId=a1 = %v, want only X'", m)
	}
	if m := albumIDsOf(in2); len(m) != 2 || !m[x.ID] || !m[y] {
		contradicted(t, "GET /albums?assetId=a2 = %v, want X' and Y", m)
	}

	// A member CAN put the sync user's assets into an album of their own (access via
	// the shared album), and ?assetId (the sync user's view) does not list that album.
	own, err := member.API.CreateAlbum(ctx, "td-own-"+suffix, "")
	must(t, err, "member album")
	res, err := member.API.AddToAlbum(ctx, own, a2)
	must(t, err, "member adds sync asset to own album")
	if len(res) != 1 || !res[0].Success {
		contradicted(t, "a member could not add the sync user's asset to their own album: %+v", res)
	}
	var in2b []testutil.Album
	must(t, sync.Do(ctx, http.MethodGet, "/albums?assetId="+a2, nil, &in2b), "albums of a2 after member add")
	if albumIDsOf(in2b)[own] {
		contradicted(t, "?assetId lists another user's unshared album")
	}

	// The member leaves X' with DELETE /albums/{id}/user/me.
	must(t, member.API.Do(ctx, http.MethodDelete, "/albums/"+x.ID+"/user/me", nil, nil), "member leaves X'")
	if _, err := member.API.GetAssetTyped(ctx, a2); err != nil {
		contradicted(t, "after leaving, the member lost access to a sync asset kept in their own album: %v", err)
	}
	if _, err := member.API.GetAssetTyped(ctx, a1); err == nil {
		contradicted(t, "after leaving, the member still reads a sync asset that was only in the left album")
	}
	got, err := sync.GetAlbum(ctx, x.ID)
	must(t, err, "get X' after leave")
	if len(got.Users) != 1 || got.Users[0].User.ID != syncID || got.Users[0].Role != "owner" {
		contradicted(t, "albumUsers after the member left = %+v, want only the owner", got.Users)
	}

	// The owner cannot leave its own album.
	if err := sync.Do(ctx, http.MethodDelete, "/albums/"+y+"/user/me", nil, nil); err == nil {
		contradicted(t, "owner left its own album")
	} else {
		t.Logf("owner leave: status %d %q", testutil.StatusOf(err), testutil.ErrMessage(err))
	}

	// Trash with force=false: the asset stays, trashed; X' no longer lists it.
	must(t, sync.Do(ctx, http.MethodDelete, "/assets", map[string]any{"ids": []string{a1}, "force": false}, nil), "trash a1")
	ta, err := sync.GetAssetTyped(ctx, a1)
	must(t, err, "get trashed a1")
	if !ta.IsTrashed {
		contradicted(t, "a1 not trashed after DELETE /assets force=false")
	}
	ids, err := sync.AlbumAssetIDs(ctx, x.ID)
	must(t, err, "X' assets after trash")
	for _, id := range ids {
		if id == a1 {
			contradicted(t, "album search lists a trashed asset")
		}
	}

	// Deleting an album keeps its assets.
	must(t, sync.DeleteAlbum(ctx, x.ID), "delete X'")
	a2x, err := sync.GetAssetTyped(ctx, a2)
	must(t, err, "a2 after album delete")
	if a2x.IsTrashed {
		contradicted(t, "deleting an album trashed its asset")
	}
	if _, err := sync.GetAlbum(ctx, x.ID); err == nil || testutil.StatusOf(err) != 400 {
		contradicted(t, "GET deleted album: %v, want 400", err)
	}
	if err := sync.DeleteAlbum(ctx, x.ID); err == nil || testutil.StatusOf(err) != 400 {
		contradicted(t, "DELETE deleted album: %v, want 400", err)
	} else {
		t.Logf("delete gone album: %q", testutil.ErrMessage(err))
	}

	// The sync user (editor via share) leaves someone else's album with only albumUser.delete.
	z, err := member.API.CreateAlbum(ctx, "td-z-"+suffix, "")
	must(t, err, "member album Z")
	must(t, member.API.ShareAlbum(ctx, z, syncID, "editor"), "share Z with sync")
	must(t, sync.Do(ctx, http.MethodDelete, "/albums/"+z+"/user/me", nil, nil), "sync leaves Z")
	if _, err := sync.GetAlbum(ctx, z); err == nil || testutil.StatusOf(err) != 400 {
		contradicted(t, "GET album after leaving: %v, want 400", err)
	}
	if err := sync.Do(ctx, http.MethodDelete, "/albums/"+z+"/user/me", nil, nil); err == nil || testutil.StatusOf(err) != 400 {
		contradicted(t, "leaving again: %v, want 400", err)
	} else {
		t.Logf("leave again: %q", testutil.ErrMessage(err))
	}
	zz, err := member.API.GetAlbum(ctx, z)
	must(t, err, "Z after sync left")
	if len(zz.Users) != 1 {
		contradicted(t, "Z albumUsers after sync left = %+v", zz.Users)
	}
}
