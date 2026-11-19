// Command bootstrap prepares the two e2e Immich instances (admin user, admin API keys) and writes .e2e/env.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"immich-ferry/test/e2e/testutil"
)

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	urlA := getenv("IMMICH_A_URL", "http://127.0.0.1:"+getenv("IMMICH_A_PORT", "22283"))
	urlB := getenv("IMMICH_B_URL", "http://127.0.0.1:"+getenv("IMMICH_B_PORT", "22284"))
	outPath := getenv("E2E_ENV_FILE", ".e2e/env")

	var keys [2]string
	for i, u := range []string{urlA, urlB} {
		in, err := testutil.Bootstrap(ctx, u)
		if err != nil {
			fatal(err)
		}
		// A fresh admin key each run keeps the command idempotent without having to read old secrets.
		key, err := in.CreateAPIKey(ctx, in.AdminToken, []string{"all"})
		if err != nil {
			fatal(err)
		}
		keys[i] = key
	}
	content := fmt.Sprintf("IMMICH_A_URL=%s\nIMMICH_B_URL=%s\nE2E_ADMIN_EMAIL=%s\nE2E_ADMIN_PASSWORD=%s\nE2E_A_KEY=%s\nE2E_B_KEY=%s\n",
		urlA, urlB, testutil.AdminEmail, testutil.AdminPassword, keys[0], keys[1])
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(outPath, []byte(content), 0o600); err != nil {
		fatal(err)
	}
	fmt.Printf("Immich A: %s\nImmich B: %s\nLogin:    %s / %s\nEnv file: %s\n", urlA, urlB, testutil.AdminEmail, testutil.AdminPassword, outPath)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bootstrap:", err)
	os.Exit(1)
}
