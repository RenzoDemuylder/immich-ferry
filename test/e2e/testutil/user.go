package testutil

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

var seedCounter atomic.Int64

func init() { seedCounter.Store(time.Now().UnixNano() % 1_000_000_000) }

// UniqueSeed returns a process-unique seed for JPEG/MP4/MOV generation.
func UniqueSeed() int64 { return seedCounter.Add(1) }

// User is a freshly created non-admin user. API authenticates with the user's bearer token (all permissions),
// which tests use for setup and assertions; the engine under test gets a separate minimal API key (Key).
type User struct {
	Inst  *Instance
	ID    string
	Email string
	Token string
	API   *API
}

// NewUser creates a unique user on inst.
func NewUser(ctx context.Context, inst *Instance, tag string) (*User, error) {
	email := fmt.Sprintf("%s-%d@immich-ferry.test", tag, UniqueSeed())
	id, tok, err := inst.CreateUser(ctx, email, "user-password-1", "E2E "+tag)
	if err != nil {
		return nil, err
	}
	return &User{Inst: inst, ID: id, Email: email, Token: tok, API: &API{URL: inst.URL, Token: tok}}, nil
}

// Key creates an API key with exactly perms and returns the secret.
func (u *User) Key(ctx context.Context, perms []string) (string, error) {
	return u.Inst.CreateAPIKey(ctx, u.Token, perms)
}

// ChecksumsOf returns checksum -> asset for the non-trashed timeline/archive assets of an album.
func ChecksumsOf(ctx context.Context, api *API, albumID string) (map[string]Asset, error) {
	as, err := api.AlbumAssets(ctx, albumID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]Asset, len(as))
	for _, a := range as {
		m[a.Checksum] = a
	}
	return m, nil
}

// ProbeChecksum returns album members with the given checksum, including hidden and trashed ones.
func ProbeChecksum(ctx context.Context, api *API, albumID, checksum string) ([]Asset, error) {
	return api.SearchStructured(ctx, map[string]any{
		"filter": map[string]any{
			"albumIds": map[string]any{"any": []string{albumID}},
			"checksum": map[string]any{"in": []string{checksum}},
		},
		"withExif": true,
	})
}

// FreeBox returns an ISO-BMFF `free` box with n payload bytes (to pad a fixture to a large size).
func FreeBox(n int) []byte {
	b := make([]byte, 8+n)
	binary.BigEndian.PutUint32(b[0:4], uint32(8+n))
	copy(b[4:8], "free")
	return b
}

// WaitAlbumHas waits until the album (as seen through api) contains an asset with the checksum.
func WaitAlbumHas(t testing.TB, api *API, albumID, checksum string, timeout time.Duration) Asset {
	t.Helper()
	var found Asset
	Eventually(t, timeout, 2*time.Second, func() (bool, string) {
		m, err := ChecksumsOf(context.Background(), api, albumID)
		if err != nil {
			return false, err.Error()
		}
		if a, ok := m[checksum]; ok {
			found = a
			return true, ""
		}
		return false, fmt.Sprintf("album %s has %d assets, checksum %s missing", albumID, len(m), checksum)
	})
	return found
}
