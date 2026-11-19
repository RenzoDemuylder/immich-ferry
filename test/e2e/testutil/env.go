package testutil

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// E2EEnv returns the e2e environment: process env first, then the repo's .e2e/env file written by `make e2e-up`.
// Keys: IMMICH_A_URL, IMMICH_B_URL, E2E_A_KEY, E2E_B_KEY, E2E_ADMIN_EMAIL, E2E_ADMIN_PASSWORD.
func E2EEnv() map[string]string {
	out := map[string]string{}
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".e2e", "env")
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok {
				out[k] = v
			}
		}
	}
	for _, k := range []string{"IMMICH_A_URL", "IMMICH_B_URL", "E2E_A_KEY", "E2E_B_KEY", "E2E_ADMIN_EMAIL", "E2E_ADMIN_PASSWORD"} {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}
