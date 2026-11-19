// Command demo uploads a few generated photos to Immich A, creates a new album with them on A and an empty album
// on B (immich-ferry never creates albums) using the API keys from .e2e/env, and prints
// SYNC_A_ALBUM_ID/SYNC_B_ALBUM_ID assignments on stdout for `make demo` to eval.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"immich-ferry/test/e2e/testutil"
)

func main() {
	env := testutil.E2EEnv()
	if env["IMMICH_A_URL"] == "" || env["E2E_A_KEY"] == "" || env["IMMICH_B_URL"] == "" || env["E2E_B_KEY"] == "" {
		fmt.Fprintln(os.Stderr, "demo: run `make e2e-up` first (.e2e/env missing)")
		os.Exit(1)
	}
	name := os.Getenv("DEMO_ALBUM")
	if name == "" {
		name = "immich-ferry demo"
	}
	ctx := context.Background()
	api := &testutil.API{URL: env["IMMICH_A_URL"], Key: env["E2E_A_KEY"]}
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	var ids []string
	for i := 0; i < 5; i++ {
		taken := base.Add(time.Duration(i) * time.Hour)
		lat, lon := 48.85+float64(i)*0.1, 2.35+float64(i)*0.1
		data := testutil.JPEG(testutil.JPEGOpts{Seed: testutil.UniqueSeed() * 7919, DateTimeOriginal: &taken, OffsetTime: "+00:00",
			Lat: &lat, Lon: &lon, Width: 640, Height: 480})
		id, _, err := api.Upload(ctx, fmt.Sprintf("demo-%d.jpg", i+1), data, testutil.UploadOpts{FileCreatedAt: taken})
		if err != nil {
			fatal(err)
		}
		ids = append(ids, id)
	}
	albumID, err := api.CreateAlbum(ctx, name, "created by make demo", ids...)
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "demo: album %q (%s) on A with %d new photos\n", name, albumID, len(ids))

	apiB := &testutil.API{URL: env["IMMICH_B_URL"], Key: env["E2E_B_KEY"]}
	albumB, err := apiB.CreateAlbum(ctx, name, "")
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "demo: album %q (%s) on B\n", name, albumB)
	fmt.Printf("SYNC_A_ALBUM_ID=%s\nSYNC_B_ALBUM_ID=%s\n", albumID, albumB)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "demo:", err)
	os.Exit(1)
}
