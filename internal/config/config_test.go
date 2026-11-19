package config

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"immich-ferry/internal/engine"
)

const (
	id1 = "11111111-1111-4111-8111-111111111111"
	id2 = "22222222-2222-4222-8222-222222222222"
	id3 = "33333333-3333-4333-8333-333333333333"
	id4 = "44444444-4444-4444-8444-444444444444"
)

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func filesOf(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return nil, fmt.Errorf("no such file %s", p)
	}
}

func baseEnv() map[string]string {
	return map[string]string{
		"IMMICH_A_URL": "http://a.example", "IMMICH_A_API_KEY": "secret-key-a",
		"IMMICH_B_URL": "https://b.example/api", "IMMICH_B_API_KEY": "secret-key-b",
		"SYNC_A_ALBUM_ID": id1, "SYNC_B_ALBUM_ID": id2,
	}
}

func TestDefaultsEnvOnly(t *testing.T) {
	c, err := Load(envOf(baseEnv()), filesOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Engine
	if c.Interval != 15*time.Minute || c.StateDir != "/data" || c.HTTPAddr != ":8080" || c.LogLevel != slog.LevelInfo || c.LogFormat != "json" {
		t.Fatalf("defaults: %+v", c)
	}
	if o.Concurrency != 4 || !o.PropagateRemovals || o.MaxRemovalRatio != 0.2 || o.MaxRemovalsMin != 10 || o.ConflictWinner != engine.A ||
		!o.SyncMetadata || !o.SyncAlbumInfo || !o.RestoreTrashed || o.PendingMetaTimeout != time.Hour ||
		o.CopyPhaseBudget != 20*time.Minute || o.CheckpointEvery != 100 || o.CheckpointInterval != 30*time.Second ||
		o.BackoffBase != 15*time.Minute || o.BackoffMax != 24*time.Hour || o.BlockedRecheck != 24*time.Hour || o.Now == nil ||
		o.TmpBudgetBytes != 0 || o.DryRun || o.TmpDir == "" {
		t.Fatalf("engine defaults: %+v", o)
	}
	if len(c.Pairs) != 1 || c.Pairs[0].Name != "default" || c.Pairs[0].A.ID != id1 || c.Pairs[0].B.ID != id2 {
		t.Fatalf("pairs: %+v", c.Pairs)
	}
	if c.Instances[0].APIKey != "secret-key-a" || c.Instances[1].URL != "https://b.example/api" {
		t.Fatalf("instances: %+v", c.Instances)
	}
}

const yamlCfg = `
instances:
  a: {url: "http://a.yaml", apiKey: yaml-a, insecureTLS: true}
  b: {url: "http://b.yaml", apiKeyFile: /keys/b}
sync:
  interval: 30m
  concurrency: 8
  tmpBudget: 20Gi
  dryRun: true
  propagateRemovals: false
  maxRemovalRatio: 0.5
  maxRemovalsMin: 3
  conflictWinner: b
  copyBudget: 5m
stateDir: /var/state
httpAddr: ":9090"
log: {level: debug, format: text}
pairs:
  - name: family
    a: {albumId: ` + id1 + `}
    b: {albumId: ` + id2 + `}
  - name: trips
    a: {albumId: ` + id3 + `}
    b: {albumId: ` + id4 + `}
`

func TestYAMLOnly(t *testing.T) {
	c, err := Load(envOf(map[string]string{"SYNC_CONFIG_FILE": "/c.yaml"}), filesOf(map[string]string{"/c.yaml": yamlCfg, "/keys/b": "  keyb\n"}))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Engine
	if c.Interval != 30*time.Minute || o.Concurrency != 8 || o.TmpBudgetBytes != 20<<30 || !o.DryRun || o.PropagateRemovals ||
		o.MaxRemovalRatio != 0.5 || o.MaxRemovalsMin != 3 || o.ConflictWinner != engine.B || o.CopyPhaseBudget != 5*time.Minute ||
		c.StateDir != "/var/state" || c.HTTPAddr != ":9090" || c.LogLevel != slog.LevelDebug || c.LogFormat != "text" {
		t.Fatalf("%+v", c)
	}
	if c.Instances[0].APIKey != "yaml-a" || !c.Instances[0].InsecureTLS || c.Instances[1].APIKey != "keyb" {
		t.Fatalf("%+v", c.Instances)
	}
	if len(c.Pairs) != 2 {
		t.Fatalf("%+v", c.Pairs)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	env := map[string]string{
		"SYNC_CONFIG_FILE": "/c.yaml", "SYNC_INTERVAL": "1m", "IMMICH_A_API_KEY": "envkey", "SYNC_DRY_RUN": "no",
		"SYNC_PAIR_NAME": "family", "SYNC_B_ALBUM_ID": id3, "LOG_LEVEL": "warn", "SYNC_TMP_BUDGET": "500Mi",
		"IMMICH_B_API_KEY_FILE": "/keys/b2",
	}
	c, err := Load(envOf(env), filesOf(map[string]string{"/c.yaml": yamlCfg, "/keys/b": "x", "/keys/b2": "kb2\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != time.Minute || c.Instances[0].APIKey != "envkey" || c.Engine.DryRun || c.LogLevel != slog.LevelWarn ||
		c.Engine.TmpBudgetBytes != 500<<20 || c.Instances[1].APIKey != "kb2" {
		t.Fatalf("%+v", c)
	}
	if len(c.Pairs) != 2 || c.Pairs[0].B.ID != id3 || c.Pairs[0].A.ID != id1 {
		t.Fatalf("pairs %+v", c.Pairs)
	}
	// Different pair name appends.
	env["SYNC_PAIR_NAME"] = "extra"
	env["SYNC_A_ALBUM_ID"] = id4
	c, err = Load(envOf(env), filesOf(map[string]string{"/c.yaml": yamlCfg, "/keys/b2": "k"}))
	if err == nil {
		if len(c.Pairs) != 3 {
			t.Fatalf("pairs %+v", c.Pairs)
		}
	}
}

func TestYAMLStrict(t *testing.T) {
	_, err := Load(envOf(map[string]string{"SYNC_CONFIG_FILE": "/c"}), filesOf(map[string]string{"/c": "bogus: 1\n"}))
	if err == nil || !errors.Is(err, engine.ErrConfig) || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidationAggregated(t *testing.T) {
	env := map[string]string{
		"IMMICH_A_URL": "ftp://x", "IMMICH_B_URL": "nohost",
		"SYNC_INTERVAL": "5s", "SYNC_CONCURRENCY": "99", "SYNC_MAX_REMOVAL_RATIO": "0", "SYNC_MAX_REMOVALS_MIN": "-1",
		"SYNC_CONFLICT_WINNER": "c", "LOG_LEVEL": "loud", "LOG_FORMAT": "xml", "SYNC_DRY_RUN": "maybe", "SYNC_COPY_BUDGET": "soon",
		"SYNC_A_ALBUM_ID": "not-a-uuid", "SYNC_PAIR_NAME": "Bad_Name",
	}
	_, err := Load(envOf(env), filesOf(nil))
	if err == nil || !errors.Is(err, engine.ErrConfig) {
		t.Fatalf("err=%v", err)
	}
	for _, want := range []string{"url \"ftp://x\"", "url \"nohost\"", "API key is empty", "at least", "concurrency", "maxRemovalRatio",
		"maxRemovalsMin", "SYNC_CONFLICT_WINNER", "log level", "log format", "SYNC_DRY_RUN", "SYNC_COPY_BUDGET",
		"name must match", "albumId must be set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestPairValidation(t *testing.T) {
	y := `
pairs:
  - {name: p1, a: {albumId: ` + id1 + `}, b: {albumId: ` + id2 + `}}
  - {name: p1, a: {albumId: ` + id2 + `}, b: {albumId: ` + id3 + `}}
  - {name: p3, a: {albumId: ` + id1 + `}, b: {albumId: ` + id4 + `}}
  - {name: p4, a: {albumId: bad}, b: {}}
`
	env := baseEnv()
	delete(env, "SYNC_A_ALBUM_ID")
	delete(env, "SYNC_B_ALBUM_ID")
	env["SYNC_CONFIG_FILE"] = "/c"
	_, err := Load(envOf(env), filesOf(map[string]string{"/c": y}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"duplicate pair name", "already used by pair", "not a UUID", "p4\") side b: albumId must be set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	// Same id on different sides is fine.
	y2 := `
pairs:
  - {name: p1, a: {albumId: ` + id1 + `}, b: {albumId: ` + id2 + `}}
  - {name: p2, a: {albumId: ` + id2 + `}, b: {albumId: ` + id1 + `}}
`
	if _, err := Load(envOf(env), filesOf(map[string]string{"/c": y2})); err != nil {
		t.Fatal(err)
	}
}

func TestNoPairsNoKeys(t *testing.T) {
	_, err := Load(envOf(nil), filesOf(nil))
	if err == nil || !strings.Contains(err.Error(), "no albums selected") {
		t.Fatalf("%v", err)
	}
}

func TestDiscoverySettings(t *testing.T) {
	noPair := func(extra map[string]string) map[string]string {
		env := baseEnv()
		delete(env, "SYNC_A_ALBUM_ID")
		delete(env, "SYNC_B_ALBUM_ID")
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	for _, tc := range []struct {
		name       string
		env        map[string]string
		yaml       string
		wantMode   engine.DiscoveryMode
		wantMarker string
		wantErr    string
	}{
		{name: "default off needs pairs", env: noPair(nil), wantErr: "no albums selected"},
		{name: "all", env: noPair(map[string]string{"SYNC_DISCOVERY": "All"}), wantMode: engine.DiscoveryAll, wantMarker: DefaultDiscoveryMarker},
		{name: "marker default", env: noPair(map[string]string{"SYNC_DISCOVERY": "marker"}), wantMode: engine.DiscoveryMarker, wantMarker: "🔁"},
		{name: "marker custom trimmed", env: noPair(map[string]string{"SYNC_DISCOVERY": "marker", "SYNC_DISCOVERY_MARKER": " #sync "}),
			wantMode: engine.DiscoveryMarker, wantMarker: "#sync"},
		{name: "empty env marker keeps default", env: noPair(map[string]string{"SYNC_DISCOVERY": "marker", "SYNC_DISCOVERY_MARKER": "  "}),
			wantMode: engine.DiscoveryMarker, wantMarker: "🔁"},
		{name: "marker empty", env: noPair(map[string]string{"SYNC_CONFIG_FILE": "/c"}),
			yaml: "sync: {discovery: marker, discoveryMarker: \" \"}\n", wantErr: "discoveryMarker must not be empty"},
		{name: "invalid", env: noPair(map[string]string{"SYNC_DISCOVERY": "some"}), wantErr: `SYNC_DISCOVERY: "some" invalid`},
		{name: "with env pair", env: func() map[string]string { e := baseEnv(); e["SYNC_DISCOVERY"] = "all"; return e }(),
			wantErr: "mutually exclusive"},
		{name: "explicit off with pair", env: func() map[string]string { e := baseEnv(); e["SYNC_DISCOVERY"] = "off"; return e }()},
		{name: "yaml", env: noPair(map[string]string{"SYNC_CONFIG_FILE": "/c"}),
			yaml: "sync: {discovery: marker, discoveryMarker: \"⭐\"}\n", wantMode: engine.DiscoveryMarker, wantMarker: "⭐"},
		{name: "env overrides yaml", env: noPair(map[string]string{"SYNC_CONFIG_FILE": "/c", "SYNC_DISCOVERY": "all"}),
			yaml: "sync: {discovery: marker}\n", wantMode: engine.DiscoveryAll, wantMarker: "🔁"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(envOf(tc.env), filesOf(map[string]string{"/c": tc.yaml}))
			if tc.wantErr != "" {
				if err == nil || !errors.Is(err, engine.ErrConfig) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Engine.Discovery != tc.wantMode || (tc.wantMarker != "" && c.Engine.DiscoveryMarker != tc.wantMarker) {
				t.Fatalf("mode %q marker %q", c.Engine.Discovery, c.Engine.DiscoveryMarker)
			}
		})
	}
}

func TestSecretsNeverInErrorsAndRedacted(t *testing.T) {
	env := baseEnv()
	env["SYNC_INTERVAL"] = "bad"
	_, err := Load(envOf(env), filesOf(nil))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("leak: %v", err)
	}
	c, err := Load(envOf(baseEnv()), filesOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	r := fmt.Sprintf("%v", Redacted(c))
	if strings.Contains(r, "secret-key") || !strings.Contains(r, "***") {
		t.Fatalf("redacted: %s", r)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"0": 0, "1024": 1024, "20Gi": 20 << 30, "500Mi": 500 << 20, "10G": 10e9, "1.5GiB": 3 << 29, "2k": 2000, "1Ki": 1024, "7B": 7,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("%q: got %d, %v want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "10X", "-5", "1..2G"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestBools(t *testing.T) {
	for _, s := range []string{"true", "1", "yes", "TRUE"} {
		if b, err := ParseBool(s); err != nil || !b {
			t.Errorf("%s", s)
		}
	}
	for _, s := range []string{"false", "0", "no"} {
		if b, err := ParseBool(s); err != nil || b {
			t.Errorf("%s", s)
		}
	}
}

func TestStateDirHelper(t *testing.T) {
	d, err := StateDir(envOf(map[string]string{"SYNC_CONFIG_FILE": "/c"}), filesOf(map[string]string{"/c": "stateDir: /x\n"}))
	if err != nil || d != "/x" {
		t.Fatal(d, err)
	}
	d, _ = StateDir(envOf(map[string]string{"SYNC_STATE_DIR": "/e"}), filesOf(nil))
	if d != "/e" {
		t.Fatal(d)
	}
	d, _ = StateDir(envOf(nil), filesOf(nil))
	if d != "/data" {
		t.Fatal(d)
	}
}

func TestDiscoveryShare(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	noPair := func(extra map[string]string) map[string]string {
		e := baseEnv()
		delete(e, "SYNC_A_ALBUM_ID")
		delete(e, "SYNC_B_ALBUM_ID")
		e["SYNC_DISCOVERY"] = "all"
		for k, v := range extra {
			e[k] = v
		}
		return e
	}
	for _, tc := range []struct {
		name      string
		env       map[string]string
		yaml      string
		want      [2][]string
		wantRole  string
		wantTrash bool
		wantDedic bool
		wantOff   bool
		wantErr   string
	}{
		{name: "default", env: noPair(nil), wantRole: "editor"},
		{name: "env lists trimmed", env: noPair(map[string]string{"SYNC_DISCOVERY_SHARE_A": " me@x.org , ," + id,
			"SYNC_DISCOVERY_SHARE_B": "you@y.org", "SYNC_DISCOVERY_SHARE_ROLE": "Viewer"}),
			want: [2][]string{{"me@x.org", id}, {"you@y.org"}}, wantRole: "viewer"},
		{name: "empty env is unset", env: noPair(map[string]string{"SYNC_DISCOVERY_SHARE_A": " ", "SYNC_DISCOVERY_SHARE_ROLE": ""}),
			wantRole: "editor"},
		{name: "yaml", env: noPair(map[string]string{"SYNC_CONFIG_FILE": "/c"}),
			yaml: "sync: {discoveryShare: {a: [me@x.org], b: [\"" + id + "\"], role: viewer}}\n",
			want: [2][]string{{"me@x.org"}, {id}}, wantRole: "viewer"},
		{name: "env overrides yaml", env: noPair(map[string]string{"SYNC_CONFIG_FILE": "/c", "SYNC_DISCOVERY_SHARE_A": "env@x.org"}),
			yaml: "sync: {discoveryShare: {a: [yaml@x.org], b: [b@x.org]}}\n",
			want: [2][]string{{"env@x.org"}, {"b@x.org"}}, wantRole: "editor"},
		{name: "bad entry", env: noPair(map[string]string{"SYNC_DISCOVERY_SHARE_B": "alice"}), wantErr: `"alice" is neither`},
		{name: "bad role", env: noPair(map[string]string{"SYNC_DISCOVERY_SHARE_ROLE": "owner"}), wantErr: `role "owner" invalid`},
		{name: "needs discovery", env: func() map[string]string { e := baseEnv(); e["SYNC_DISCOVERY_SHARE_A"] = "me@x.org"; return e }(),
			wantErr: "only used with album discovery"},
		{name: "teardown trash env", env: noPair(map[string]string{"SYNC_DISCOVERY_SHARE_B": "you@y.org", "SYNC_DISCOVERY_TEARDOWN_TRASH": "true"}),
			want: [2][]string{nil, {"you@y.org"}}, wantRole: "editor", wantTrash: true},
		{name: "teardown trash yaml", env: noPair(map[string]string{"SYNC_CONFIG_FILE": "/c"}),
			yaml: "sync: {discoveryShare: {b: [you@y.org], trashOnTeardown: true}}\n",
			want: [2][]string{nil, {"you@y.org"}}, wantRole: "editor", wantTrash: true},
		{name: "dedicated user env", env: noPair(map[string]string{"SYNC_DISCOVERY_SHARE_B": "you@y.org", "SYNC_DISCOVERY_DEDICATED_USER": "true"}),
			want: [2][]string{nil, {"you@y.org"}}, wantRole: "editor", wantDedic: true},
		{name: "dedicated user yaml", env: noPair(map[string]string{"SYNC_CONFIG_FILE": "/c"}),
			yaml: "sync: {discoveryShare: {b: [you@y.org], dedicatedUser: true}}\n",
			want: [2][]string{nil, {"you@y.org"}}, wantRole: "editor", wantDedic: true},
		{name: "dedicated user needs share", env: noPair(map[string]string{"SYNC_DISCOVERY_DEDICATED_USER": "true"}),
			wantErr: "only applies to shared albums"},
		{name: "teardown off env", env: noPair(map[string]string{"SYNC_DISCOVERY_SHARE_B": "you@y.org", "SYNC_DISCOVERY_TEARDOWN": "false"}),
			want: [2][]string{nil, {"you@y.org"}}, wantRole: "editor", wantOff: true},
		{name: "teardown off yaml", env: noPair(map[string]string{"SYNC_CONFIG_FILE": "/c"}),
			yaml: "sync: {discoveryShare: {b: [you@y.org], teardown: false}}\n",
			want: [2][]string{nil, {"you@y.org"}}, wantRole: "editor", wantOff: true},
		{name: "teardown env overrides yaml", env: noPair(map[string]string{"SYNC_CONFIG_FILE": "/c", "SYNC_DISCOVERY_TEARDOWN": "true"}),
			yaml: "sync: {discoveryShare: {b: [you@y.org], teardown: false}}\n",
			want: [2][]string{nil, {"you@y.org"}}, wantRole: "editor"},
		{name: "teardown trash needs teardown", env: noPair(map[string]string{"SYNC_DISCOVERY_SHARE_B": "you@y.org",
			"SYNC_DISCOVERY_TEARDOWN": "false", "SYNC_DISCOVERY_TEARDOWN_TRASH": "true"}), wantErr: "needs the teardown"},
		{name: "teardown trash needs share", env: noPair(map[string]string{"SYNC_DISCOVERY_TEARDOWN_TRASH": "true"}),
			wantErr: "only applies to shared albums"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(envOf(tc.env), filesOf(map[string]string{"/c": tc.yaml}))
			if tc.wantErr != "" {
				if err == nil || !errors.Is(err, engine.ErrConfig) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.Engine.DiscoveryShare, tc.want) || c.Engine.DiscoveryShareRole != tc.wantRole ||
				c.Engine.DiscoveryTeardownTrash != tc.wantTrash || c.Engine.DiscoveryDedicatedUser != tc.wantDedic ||
				c.Engine.DiscoveryTeardownOff != tc.wantOff {
				t.Fatalf("share %q role %q", c.Engine.DiscoveryShare, c.Engine.DiscoveryShareRole)
			}
		})
	}
}
