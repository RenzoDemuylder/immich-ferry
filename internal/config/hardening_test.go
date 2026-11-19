package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func loadWith(t *testing.T, extra map[string]string) error {
	t.Helper()
	env := baseEnv()
	for k, v := range extra {
		env[k] = v
	}
	_, err := Load(envOf(env), filesOf(nil))
	return err
}

func TestRejectZeroEngineDefaults(t *testing.T) {
	for _, tc := range []struct{ k, v, want string }{
		{"SYNC_MAX_REMOVALS_MIN", "0", "maxRemovalsMin"},
		{"SYNC_PENDING_META_TIMEOUT", "0s", "pendingMetaTimeout"},
		{"SYNC_COPY_BUDGET", "0s", "copyBudget"},
	} {
		if err := loadWith(t, map[string]string{tc.k: tc.v}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%s: err=%v", tc.k, tc.v, err)
		}
	}
}

func TestAPIKeyWhitespaceRejected(t *testing.T) {
	files := map[string]string{"/k": "line1\nline2\n"}
	env := baseEnv()
	delete(env, "IMMICH_A_API_KEY")
	env["IMMICH_A_API_KEY_FILE"] = "/k"
	_, err := Load(envOf(env), filesOf(files))
	if err == nil || !strings.Contains(err.Error(), "whitespace") || strings.Contains(err.Error(), "line1") {
		t.Fatalf("err=%v", err)
	}
	if err := loadWith(t, map[string]string{"IMMICH_B_API_KEY": "ab cd"}); err == nil {
		t.Fatal("space in env key accepted")
	}
	// trailing newline alone is fine (trimmed)
	files["/k"] = "goodkey\n"
	if _, err := Load(envOf(env), filesOf(files)); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPAddrValidated(t *testing.T) {
	for _, bad := range []string{"7000", "localhost", ":abc", ":99999"} {
		if err := loadWith(t, map[string]string{"HTTP_ADDR": bad}); err == nil || !strings.Contains(err.Error(), "httpAddr") {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	for _, ok := range []string{":8080", "127.0.0.1:9", "[::1]:8080", "0.0.0.0:80"} {
		if err := loadWith(t, map[string]string{"HTTP_ADDR": ok}); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
}

func TestRedactedURLUserinfo(t *testing.T) {
	c, err := Load(envOf(map[string]string{
		"IMMICH_A_URL": "http://bob:hunter2@a.example", "IMMICH_A_API_KEY": "secret-key-a",
		"IMMICH_B_URL": "http://tok3n@b.example", "IMMICH_B_API_KEY": "secret-key-b",
		"SYNC_A_ALBUM_ID": id1, "SYNC_B_ALBUM_ID": id2,
	}), filesOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Redacted(c))
	for _, leak := range []string{"hunter2", "tok3n", "secret-key"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("leaked %q: %s", leak, b)
		}
	}
}
