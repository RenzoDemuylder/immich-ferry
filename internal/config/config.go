// Package config loads configuration from env, YAML and *_FILE secrets.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"immich-ferry/internal/engine"
)

// Instance is the connection settings of one Immich instance.
type Instance struct {
	URL, APIKey string
	InsecureTLS bool
}

// Config is the fully resolved configuration.
type Config struct {
	Instances [2]Instance
	Pairs     []engine.Pair
	Interval  time.Duration
	Engine    engine.Options
	StateDir  string
	HTTPAddr  string
	LogFormat string
	LogLevel  slog.Level
}

// Defaults.
const (
	DefaultInterval = 15 * time.Minute
	DefaultStateDir = "/data"
	// DefaultDiscoveryMarker selects albums in discovery mode "marker".
	DefaultDiscoveryMarker = "🔁"
	// DefaultDiscoveryShareRole is the role of users albums created by discovery are shared with.
	DefaultDiscoveryShareRole = "editor"
	DefaultHTTPAddr           = ":8080"
	// MinInterval is the smallest accepted sync interval.
	MinInterval = 10 * time.Second
)

var (
	pairNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	emailRe    = regexp.MustCompile(`^[^@\s,]+@[^@\s,]+$`)
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Error aggregates all validation problems. It wraps engine.ErrConfig.
type Error struct{ Problems []string }

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("%v: %d problem(s):\n  - %s", engine.ErrConfig, len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// Is makes errors.Is(err, engine.ErrConfig) true.
func (e *Error) Is(target error) bool { return target == engine.ErrConfig }

// Load builds a Config from getenv and readFile (injected for tests).
// Errors wrap engine.ErrConfig and list every problem found; they never contain API keys.
func Load(getenv func(string) (string, bool), readFile func(string) ([]byte, error)) (*Config, error) {
	if getenv == nil {
		getenv = os.LookupEnv
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	l := &loader{getenv: getenv, readFile: readFile}
	l.load()
	if len(l.problems) > 0 {
		return nil, &Error{Problems: l.problems}
	}
	return l.cfg, nil
}

// StateDir resolves only the state directory (env, then config file, then default)
// without validating anything else. Used by admin subcommands.
func StateDir(getenv func(string) (string, bool), readFile func(string) ([]byte, error)) (string, error) {
	if v, ok := getenv("SYNC_STATE_DIR"); ok && v != "" {
		return v, nil
	}
	if p, ok := getenv("SYNC_CONFIG_FILE"); ok && p != "" {
		data, err := readFile(p)
		if err != nil {
			return "", fmt.Errorf("%w: read config file: %v", engine.ErrConfig, err)
		}
		f, err := parseFile(data)
		if err != nil {
			return "", fmt.Errorf("%w: %v", engine.ErrConfig, err)
		}
		if f.StateDir != "" {
			return f.StateDir, nil
		}
	}
	return DefaultStateDir, nil
}

// HTTPAddr resolves only the HTTP listen address (env, then config file, then default),
// best effort and without validation. Used by the healthcheck subcommand so it follows the
// same precedence as the daemon.
func HTTPAddr(getenv func(string) (string, bool), readFile func(string) ([]byte, error)) string {
	if v, ok := getenv("HTTP_ADDR"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if p, ok := getenv("SYNC_CONFIG_FILE"); ok && p != "" {
		if data, err := readFile(p); err == nil {
			if f, err := parseFile(data); err == nil && strings.TrimSpace(f.HTTPAddr) != "" {
				return strings.TrimSpace(f.HTTPAddr)
			}
		}
	}
	return DefaultHTTPAddr
}

// Redacted returns a JSON-friendly view of c with API keys masked.
func Redacted(c *Config) map[string]any {
	if c == nil {
		return nil
	}
	mask := func(k string) string {
		if k == "" {
			return ""
		}
		return "***"
	}
	inst := map[string]any{}
	for i, name := range []string{"a", "b"} {
		inst[name] = map[string]any{
			"url":         redactURL(c.Instances[i].URL),
			"apiKey":      mask(c.Instances[i].APIKey),
			"insecureTLS": c.Instances[i].InsecureTLS,
		}
	}
	pairs := make([]map[string]any, 0, len(c.Pairs))
	for _, p := range c.Pairs {
		pairs = append(pairs, map[string]any{
			"name": p.Name,
			"a":    map[string]any{"albumId": p.A.ID},
			"b":    map[string]any{"albumId": p.B.ID},
		})
	}
	o := c.Engine
	return map[string]any{
		"instances": inst,
		"pairs":     pairs,
		"interval":  c.Interval.String(),
		"stateDir":  c.StateDir,
		"httpAddr":  c.HTTPAddr,
		"log":       map[string]any{"level": c.LogLevel.String(), "format": c.LogFormat},
		"sync": map[string]any{
			"concurrency":        o.Concurrency,
			"tmpDir":             o.TmpDir,
			"tmpBudgetBytes":     o.TmpBudgetBytes,
			"dryRun":             o.DryRun,
			"propagateRemovals":  o.PropagateRemovals,
			"maxRemovalRatio":    o.MaxRemovalRatio,
			"maxRemovalsMin":     o.MaxRemovalsMin,
			"conflictWinner":     o.ConflictWinner.String(),
			"metadata":           o.SyncMetadata,
			"albumInfo":          o.SyncAlbumInfo,
			"restoreTrashed":     o.RestoreTrashed,
			"copyBudget":         o.CopyPhaseBudget.String(),
			"pendingMetaTimeout": o.PendingMetaTimeout.String(),
			"discovery":          discoveryName(o.Discovery),
			"discoveryMarker":    o.DiscoveryMarker,
			"discoveryShare": map[string]any{
				"a": nonNil(o.DiscoveryShare[0]), "b": nonNil(o.DiscoveryShare[1]), "role": o.DiscoveryShareRole,
				"teardown":        !o.DiscoveryTeardownOff,
				"trashOnTeardown": o.DiscoveryTeardownTrash,
				"dedicatedUser":   o.DiscoveryDedicatedUser,
			},
		},
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func discoveryName(m engine.DiscoveryMode) string {
	if m == engine.DiscoveryOff {
		return "off"
	}
	return string(m)
}

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// redactURL masks any userinfo password in a URL (u.Redacted); unparsable input is
// replaced entirely so credentials never leak.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "<invalid url>"
	}
	if u.User != nil {
		if _, has := u.User.Password(); !has {
			u.User = url.User("xxxxx") // a username-only userinfo may itself be a token
		}
	}
	return u.Redacted()
}
