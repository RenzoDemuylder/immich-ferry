//go:build e2e || e2e_kind

package e2e

import (
	"context"
	"testing"
	"time"

	"immich-ferry/internal/engine"
	"immich-ferry/test/e2e/testutil"
)

// media is an uploaded asset together with its bytes.
type media struct {
	ID, Sum, Name string
	Data          []byte
}

func upload(t *testing.T, u *testutil.User, name string, data []byte, uo testutil.UploadOpts) media {
	t.Helper()
	id, dup, err := u.API.Upload(context.Background(), name, data, uo)
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	if dup {
		t.Fatalf("upload %s: unexpected duplicate (id %s)", name, id)
	}
	return media{ID: id, Sum: testutil.SHA1Base64(data), Name: name, Data: data}
}

// photoNoWait uploads a unique JPEG with EXIF date == created (UTC).
func photoNoWait(t *testing.T, u *testutil.User, name string, created time.Time) media {
	t.Helper()
	data := testutil.JPEG(testutil.JPEGOpts{Seed: testutil.UniqueSeed(), DateTimeOriginal: &created, OffsetTime: "+00:00"})
	return upload(t, u, name, data, testutil.UploadOpts{FileCreatedAt: created})
}

func photo(t *testing.T, u *testutil.User, name string, created time.Time) media {
	t.Helper()
	m := photoNoWait(t, u, name, created)
	testutil.WaitExtracted(t, u.API, m.ID)
	return m
}

func ids(ms ...media) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func defaultOpts(t *testing.T) engine.Options {
	return engine.Options{Concurrency: 2, TmpDir: t.TempDir(), PropagateRemovals: true, MaxRemovalRatio: 0.2,
		MaxRemovalsMin: 10, ConflictWinner: engine.A, SyncMetadata: true, SyncAlbumInfo: true,
		RestoreTrashed: true}
}

var t0 = time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

type statusDoc struct {
	Current struct {
		Phase string `json:"phase"`
	} `json:"current"`
	Pairs map[string]struct {
		Result    string
		Started   time.Time
		Finished  time.Time
		Mutations int
		Pending   int
		Bootstrap bool
	} `json:"pairs"`
	Errors map[string]string `json:"errors"`
}
