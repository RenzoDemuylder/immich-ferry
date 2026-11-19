package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"immich-ferry/internal/engine"
)

type fileInstance struct {
	URL         string `yaml:"url"`
	APIKey      string `yaml:"apiKey"`
	APIKeyFile  string `yaml:"apiKeyFile"`
	InsecureTLS *bool  `yaml:"insecureTLS"`
}

type fileSync struct {
	Interval           string    `yaml:"interval"`
	Concurrency        *int      `yaml:"concurrency"`
	TmpDir             string    `yaml:"tmpDir"`
	TmpBudget          yaml.Node `yaml:"tmpBudget"`
	DryRun             *bool     `yaml:"dryRun"`
	PropagateRemovals  *bool     `yaml:"propagateRemovals"`
	MaxRemovalRatio    *float64  `yaml:"maxRemovalRatio"`
	MaxRemovalsMin     *int      `yaml:"maxRemovalsMin"`
	ConflictWinner     string    `yaml:"conflictWinner"`
	Metadata           *bool     `yaml:"metadata"`
	AlbumInfo          *bool     `yaml:"albumInfo"`
	RestoreTrashed     *bool     `yaml:"restoreTrashed"`
	CopyBudget         string    `yaml:"copyBudget"`
	PendingMetaTimeout string    `yaml:"pendingMetaTimeout"`
	Discovery          string    `yaml:"discovery"`
	DiscoveryMarker    *string   `yaml:"discoveryMarker"`
	DiscoveryShare     struct {
		A    []string `yaml:"a"`
		B    []string `yaml:"b"`
		Role string   `yaml:"role"`
		// TrashOnTeardown: see SYNC_DISCOVERY_TEARDOWN_TRASH.
		TrashOnTeardown *bool `yaml:"trashOnTeardown"`
		// DedicatedUser: see SYNC_DISCOVERY_DEDICATED_USER.
		DedicatedUser *bool `yaml:"dedicatedUser"`
	} `yaml:"discoveryShare"`
}

type fileAlbum struct {
	AlbumID string `yaml:"albumId"`
}

type filePair struct {
	Name string    `yaml:"name"`
	A    fileAlbum `yaml:"a"`
	B    fileAlbum `yaml:"b"`
}

type fileConfig struct {
	Instances struct {
		A fileInstance `yaml:"a"`
		B fileInstance `yaml:"b"`
	} `yaml:"instances"`
	Sync     fileSync `yaml:"sync"`
	StateDir string   `yaml:"stateDir"`
	HTTPAddr string   `yaml:"httpAddr"`
	Log      struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
	} `yaml:"log"`
	Pairs []filePair `yaml:"pairs"`
}

func parseFile(data []byte) (*fileConfig, error) {
	var f fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config file: %w", err)
	}
	return &f, nil
}

type loader struct {
	getenv   func(string) (string, bool)
	readFile func(string) ([]byte, error)
	cfg      *Config
	problems []string
}

func (l *loader) fail(format string, a ...any) {
	l.problems = append(l.problems, fmt.Sprintf(format, a...))
}

func (l *loader) env(name string) (string, bool) {
	v, ok := l.getenv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return strings.TrimSpace(v), true
}

// ParseBool accepts true/false/1/0/yes/no (case-insensitive).
func ParseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "y", "on":
		return true, nil
	case "false", "0", "no", "n", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q (want true/false/1/0/yes/no)", s)
}

// ParseSize parses a byte count: plain integer, or number with suffix
// K/M/G/T (decimal, 1000-based), Ki/Mi/Gi/Ti (binary), optionally followed by "B".
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || t[i] == '.') {
		i++
	}
	num, unit := t[:i], strings.TrimSpace(t[i:])
	if num == "" {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	u := strings.ToLower(unit)
	u = strings.TrimSuffix(u, "b")
	mult := map[string]float64{
		"": 1, "k": 1e3, "m": 1e6, "g": 1e9, "t": 1e12,
		"ki": 1 << 10, "mi": 1 << 20, "gi": 1 << 30, "ti": 1 << 40,
	}
	m, ok := mult[u]
	if !ok {
		return 0, fmt.Errorf("invalid size unit in %q", s)
	}
	v := n * m
	if v > math.MaxInt64/2 {
		return 0, fmt.Errorf("size %q too large", s)
	}
	return int64(v), nil
}

func (l *loader) load() {
	cfg := &Config{
		Interval: DefaultInterval,
		StateDir: DefaultStateDir,
		HTTPAddr: DefaultHTTPAddr,
		LogLevel: slog.LevelInfo,
		Engine: engine.Options{
			Concurrency:        4,
			TmpDir:             os.TempDir(),
			PropagateRemovals:  true,
			MaxRemovalRatio:    0.2,
			MaxRemovalsMin:     10,
			ConflictWinner:     engine.A,
			SyncMetadata:       true,
			SyncAlbumInfo:      true,
			RestoreTrashed:     true,
			PendingMetaTimeout: time.Hour,
			DiscoveryMarker:    DefaultDiscoveryMarker,
			DiscoveryShareRole: DefaultDiscoveryShareRole,
			CopyPhaseBudget:    20 * time.Minute,
			CheckpointEvery:    100,
			CheckpointInterval: 30 * time.Second,
			BackoffBase:        15 * time.Minute,
			BackoffMax:         24 * time.Hour,
			BlockedRecheck:     24 * time.Hour,
			Now:                time.Now,
		},
		LogFormat: "json",
	}
	l.cfg = cfg
	logLevel := "info"

	var f *fileConfig
	if p, ok := l.env("SYNC_CONFIG_FILE"); ok {
		data, err := l.readFile(p)
		if err != nil {
			l.fail("read SYNC_CONFIG_FILE %q: %v", p, err)
			f = &fileConfig{}
		} else if f, err = parseFile(data); err != nil {
			l.fail("%v", err)
			f = &fileConfig{}
		}
	} else {
		f = &fileConfig{}
	}

	// File layer.
	files := [2]*fileInstance{&f.Instances.A, &f.Instances.B}
	for i, fi := range files {
		side := string(rune('a' + i))
		cfg.Instances[i].URL = strings.TrimSpace(fi.URL)
		cfg.Instances[i].APIKey = strings.TrimSpace(fi.APIKey)
		if fi.APIKeyFile != "" {
			l.readKeyFile(side, fi.APIKeyFile, &cfg.Instances[i])
		}
		if fi.InsecureTLS != nil {
			cfg.Instances[i].InsecureTLS = *fi.InsecureTLS
		}
	}
	s := &f.Sync
	if s.Interval != "" {
		l.dur("sync.interval", s.Interval, &cfg.Interval)
	}
	if s.Concurrency != nil {
		cfg.Engine.Concurrency = *s.Concurrency
	}
	if s.TmpDir != "" {
		cfg.Engine.TmpDir = s.TmpDir
	}
	if s.TmpBudget.Kind == yaml.ScalarNode {
		l.size("sync.tmpBudget", s.TmpBudget.Value, &cfg.Engine.TmpBudgetBytes)
	} else if s.TmpBudget.Kind != 0 {
		l.fail("sync.tmpBudget: must be a number or size string")
	}
	setB := func(p *bool, dst *bool) {
		if p != nil {
			*dst = *p
		}
	}
	setB(s.DryRun, &cfg.Engine.DryRun)
	setB(s.PropagateRemovals, &cfg.Engine.PropagateRemovals)
	setB(s.Metadata, &cfg.Engine.SyncMetadata)
	setB(s.AlbumInfo, &cfg.Engine.SyncAlbumInfo)
	setB(s.RestoreTrashed, &cfg.Engine.RestoreTrashed)
	if s.MaxRemovalRatio != nil {
		cfg.Engine.MaxRemovalRatio = *s.MaxRemovalRatio
	}
	if s.MaxRemovalsMin != nil {
		cfg.Engine.MaxRemovalsMin = *s.MaxRemovalsMin
	}
	if s.ConflictWinner != "" {
		l.winner("sync.conflictWinner", s.ConflictWinner, &cfg.Engine.ConflictWinner)
	}
	if s.CopyBudget != "" {
		l.dur("sync.copyBudget", s.CopyBudget, &cfg.Engine.CopyPhaseBudget)
	}
	if s.PendingMetaTimeout != "" {
		l.dur("sync.pendingMetaTimeout", s.PendingMetaTimeout, &cfg.Engine.PendingMetaTimeout)
	}
	if s.Discovery != "" {
		l.discovery("sync.discovery", s.Discovery, &cfg.Engine.Discovery)
	}
	if s.DiscoveryMarker != nil {
		cfg.Engine.DiscoveryMarker = strings.TrimSpace(*s.DiscoveryMarker)
	}
	for i, list := range [2][]string{s.DiscoveryShare.A, s.DiscoveryShare.B} {
		if list != nil {
			cfg.Engine.DiscoveryShare[i] = cleanList(list)
		}
	}
	if s.DiscoveryShare.Role != "" {
		cfg.Engine.DiscoveryShareRole = strings.ToLower(strings.TrimSpace(s.DiscoveryShare.Role))
	}
	setB(s.DiscoveryShare.TrashOnTeardown, &cfg.Engine.DiscoveryTeardownTrash)
	setB(s.DiscoveryShare.DedicatedUser, &cfg.Engine.DiscoveryDedicatedUser)
	if f.StateDir != "" {
		cfg.StateDir = f.StateDir
	}
	if f.HTTPAddr != "" {
		cfg.HTTPAddr = f.HTTPAddr
	}
	if f.Log.Level != "" {
		logLevel = f.Log.Level
	}
	if f.Log.Format != "" {
		cfg.LogFormat = f.Log.Format
	}
	for _, p := range f.Pairs {
		cfg.Pairs = append(cfg.Pairs, engine.Pair{
			Name: p.Name,
			A:    engine.AlbumRef{ID: p.A.AlbumID},
			B:    engine.AlbumRef{ID: p.B.AlbumID},
		})
	}

	// Env layer.
	for i := range cfg.Instances {
		side := string(rune('A' + i))
		in := &cfg.Instances[i]
		if v, ok := l.env("IMMICH_" + side + "_URL"); ok {
			in.URL = v
		}
		if v, ok := l.env("IMMICH_" + side + "_API_KEY"); ok {
			in.APIKey = strings.TrimSpace(v)
		}
		if v, ok := l.env("IMMICH_" + side + "_API_KEY_FILE"); ok {
			l.readKeyFile(strings.ToLower(side), v, in)
		}
		l.envBool("IMMICH_"+side+"_INSECURE_TLS", &in.InsecureTLS)
	}
	if v, ok := l.env("SYNC_INTERVAL"); ok {
		l.dur("SYNC_INTERVAL", v, &cfg.Interval)
	}
	if v, ok := l.env("SYNC_CONCURRENCY"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			l.fail("SYNC_CONCURRENCY: invalid integer %q", v)
		} else {
			cfg.Engine.Concurrency = n
		}
	}
	if v, ok := l.env("SYNC_STATE_DIR"); ok {
		cfg.StateDir = v
	}
	if v, ok := l.env("SYNC_TMP_DIR"); ok {
		cfg.Engine.TmpDir = v
	}
	if v, ok := l.env("SYNC_TMP_BUDGET"); ok {
		l.size("SYNC_TMP_BUDGET", v, &cfg.Engine.TmpBudgetBytes)
	}
	l.envBool("SYNC_DRY_RUN", &cfg.Engine.DryRun)
	l.envBool("SYNC_PROPAGATE_REMOVALS", &cfg.Engine.PropagateRemovals)
	l.envBool("SYNC_METADATA", &cfg.Engine.SyncMetadata)
	l.envBool("SYNC_ALBUM_INFO", &cfg.Engine.SyncAlbumInfo)
	l.envBool("SYNC_RESTORE_TRASHED", &cfg.Engine.RestoreTrashed)
	if v, ok := l.env("SYNC_MAX_REMOVAL_RATIO"); ok {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil {
			l.fail("SYNC_MAX_REMOVAL_RATIO: invalid number %q", v)
		} else {
			cfg.Engine.MaxRemovalRatio = r
		}
	}
	if v, ok := l.env("SYNC_MAX_REMOVALS_MIN"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			l.fail("SYNC_MAX_REMOVALS_MIN: invalid integer %q", v)
		} else {
			cfg.Engine.MaxRemovalsMin = n
		}
	}
	if v, ok := l.env("SYNC_CONFLICT_WINNER"); ok {
		l.winner("SYNC_CONFLICT_WINNER", v, &cfg.Engine.ConflictWinner)
	}
	if v, ok := l.env("SYNC_COPY_BUDGET"); ok {
		l.dur("SYNC_COPY_BUDGET", v, &cfg.Engine.CopyPhaseBudget)
	}
	if v, ok := l.env("SYNC_PENDING_META_TIMEOUT"); ok {
		l.dur("SYNC_PENDING_META_TIMEOUT", v, &cfg.Engine.PendingMetaTimeout)
	}
	if v, ok := l.env("SYNC_DISCOVERY"); ok {
		l.discovery("SYNC_DISCOVERY", v, &cfg.Engine.Discovery)
	}
	if v, ok := l.env("SYNC_DISCOVERY_MARKER"); ok {
		cfg.Engine.DiscoveryMarker = strings.TrimSpace(v)
	}
	for i, name := range [2]string{"SYNC_DISCOVERY_SHARE_A", "SYNC_DISCOVERY_SHARE_B"} {
		if v, ok := l.env(name); ok {
			cfg.Engine.DiscoveryShare[i] = cleanList(strings.Split(v, ","))
		}
	}
	if v, ok := l.env("SYNC_DISCOVERY_SHARE_ROLE"); ok {
		cfg.Engine.DiscoveryShareRole = strings.ToLower(v)
	}
	l.envBool("SYNC_DISCOVERY_TEARDOWN_TRASH", &cfg.Engine.DiscoveryTeardownTrash)
	l.envBool("SYNC_DISCOVERY_DEDICATED_USER", &cfg.Engine.DiscoveryDedicatedUser)
	if v, ok := l.env("HTTP_ADDR"); ok {
		cfg.HTTPAddr = v
	}
	if v, ok := l.env("LOG_LEVEL"); ok {
		logLevel = v
	}
	if v, ok := l.env("LOG_FORMAT"); ok {
		cfg.LogFormat = v
	}

	l.envPair()
	l.parseLevel(logLevel)
	l.validate()
}

func (l *loader) parseLevel(s string) {
	switch strings.ToLower(s) {
	case "debug":
		l.cfg.LogLevel = slog.LevelDebug
	case "info":
		l.cfg.LogLevel = slog.LevelInfo
	case "warn", "warning":
		l.cfg.LogLevel = slog.LevelWarn
	case "error":
		l.cfg.LogLevel = slog.LevelError
	default:
		l.fail("log level %q invalid (want debug|info|warn|error)", s)
	}
}

func (l *loader) envPair() {
	var a, b engine.AlbumRef
	var any bool
	get := func(n string) string {
		v, ok := l.env(n)
		if ok {
			any = true
		}
		return v
	}
	a.ID, b.ID = get("SYNC_A_ALBUM_ID"), get("SYNC_B_ALBUM_ID")
	if !any {
		return
	}
	name, ok := l.env("SYNC_PAIR_NAME")
	if !ok {
		name = "default"
	}
	for i := range l.cfg.Pairs {
		p := &l.cfg.Pairs[i]
		if p.Name != name {
			continue
		}
		// Env overrides per side.
		if a.ID != "" {
			p.A = a
		}
		if b.ID != "" {
			p.B = b
		}
		return
	}
	l.cfg.Pairs = append(l.cfg.Pairs, engine.Pair{Name: name, A: a, B: b})
}

func (l *loader) readKeyFile(side, path string, in *Instance) {
	data, err := l.readFile(path)
	if err != nil {
		l.fail("instance %s: read API key file %q: %v", side, path, err)
		return
	}
	in.APIKey = strings.TrimSpace(string(data))
}

func (l *loader) dur(name, v string, dst *time.Duration) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		l.fail("%s: invalid duration %q", name, v)
		return
	}
	*dst = d
}

func (l *loader) size(name, v string, dst *int64) {
	n, err := ParseSize(v)
	if err != nil {
		l.fail("%s: %v", name, err)
		return
	}
	*dst = n
}

func (l *loader) discovery(name, v string, dst *engine.DiscoveryMode) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off":
		*dst = engine.DiscoveryOff
	case "all":
		*dst = engine.DiscoveryAll
	case "marker":
		*dst = engine.DiscoveryMarker
	default:
		l.fail("%s: %q invalid (want off, all or marker)", name, v)
	}
}

// cleanList trims the entries of a list setting and drops empty ones.
func cleanList(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (l *loader) winner(name, v string, dst *engine.Side) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "a":
		*dst = engine.A
	case "b":
		*dst = engine.B
	default:
		l.fail("%s: %q invalid (want a or b)", name, v)
	}
}

func (l *loader) envBool(name string, dst *bool) {
	v, ok := l.env(name)
	if !ok {
		return
	}
	b, err := ParseBool(v)
	if err != nil {
		l.fail("%s: %v", name, err)
		return
	}
	*dst = b
}

func badKeyRune(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || r == 0x7f
}

func (l *loader) validate() {
	c := l.cfg
	for i, in := range c.Instances {
		side := string(rune('a' + i))
		if !validURL(in.URL) {
			l.fail("instance %s: url %q must be an absolute http(s) URL", side, in.URL)
		}
		if in.APIKey == "" {
			l.fail("instance %s: API key is empty (set IMMICH_%s_API_KEY or IMMICH_%s_API_KEY_FILE)", side, strings.ToUpper(side), strings.ToUpper(side))
		} else if i := strings.IndexFunc(in.APIKey, badKeyRune); i >= 0 {
			l.fail("instance %s: API key contains whitespace or a control character at position %d (check for a multi-line key file or stray quotes)", side, i)
		}
	}
	switch {
	case c.Engine.Discovery != engine.DiscoveryOff && len(c.Pairs) > 0:
		l.fail("album discovery (%s) and configured pairs are mutually exclusive: remove SYNC_{A,B}_ALBUM_ID / pairs, or set SYNC_DISCOVERY=off", c.Engine.Discovery)
	case c.Engine.Discovery == engine.DiscoveryOff && len(c.Pairs) == 0:
		l.fail("no albums selected: set SYNC_DISCOVERY=all or marker, or SYNC_A_ALBUM_ID and SYNC_B_ALBUM_ID, or pairs in the config file")
	}
	if c.Engine.Discovery == engine.DiscoveryMarker && c.Engine.DiscoveryMarker == "" {
		l.fail("discoveryMarker must not be empty when discovery is marker")
	}
	for i, list := range c.Engine.DiscoveryShare {
		side := string(rune('a' + i))
		if len(list) > 0 && c.Engine.Discovery == engine.DiscoveryOff {
			l.fail("discovery share for side %s (SYNC_DISCOVERY_SHARE_%s) is only used with album discovery: set SYNC_DISCOVERY=all or marker",
				side, strings.ToUpper(side))
		}
		for _, v := range list {
			if !uuidRe.MatchString(v) && !emailRe.MatchString(v) {
				l.fail("discovery share for side %s: %q is neither a user id (UUID) nor an email address", side, v)
			}
		}
	}
	if c.Engine.DiscoveryTeardownTrash && len(c.Engine.DiscoveryShare[0]) == 0 && len(c.Engine.DiscoveryShare[1]) == 0 {
		l.fail("SYNC_DISCOVERY_TEARDOWN_TRASH (discoveryShare.trashOnTeardown) only applies to shared albums: set SYNC_DISCOVERY_SHARE_A or _B")
	}
	if c.Engine.DiscoveryDedicatedUser && len(c.Engine.DiscoveryShare[0]) == 0 && len(c.Engine.DiscoveryShare[1]) == 0 {
		l.fail("SYNC_DISCOVERY_DEDICATED_USER (discoveryShare.dedicatedUser) only applies to shared albums: set SYNC_DISCOVERY_SHARE_A or _B")
	}
	if r := c.Engine.DiscoveryShareRole; r != "editor" && r != "viewer" {
		l.fail("discovery share role %q invalid (want editor or viewer)", r)
	}
	names := map[string]bool{}
	albums := map[string]string{} // side|id -> pair
	for i, p := range c.Pairs {
		label := fmt.Sprintf("pair %d (%q)", i+1, p.Name)
		if !pairNameRe.MatchString(p.Name) {
			l.fail("%s: name must match %s", label, pairNameRe)
		}
		if names[p.Name] {
			l.fail("%s: duplicate pair name", label)
		}
		names[p.Name] = true
		for si, ref := range [2]engine.AlbumRef{p.A, p.B} {
			side := string(rune('a' + si))
			if ref.ID == "" {
				l.fail("%s side %s: albumId must be set", label, side)
				continue
			}
			if !uuidRe.MatchString(ref.ID) {
				l.fail("%s side %s: albumId %q is not a UUID", label, side, ref.ID)
			}
			val := strings.ToLower(ref.ID)
			k := side + "|" + val
			if other, dup := albums[k]; dup && other != p.Name {
				l.fail("%s side %s: album id %q already used by pair %q", label, side, val, other)
			}
			if _, ok := albums[k]; !ok {
				albums[k] = p.Name
			}
		}
	}
	if c.Interval < MinInterval {
		l.fail("interval %s must be at least %s", c.Interval, MinInterval)
	}
	if c.Engine.Concurrency < 1 || c.Engine.Concurrency > 32 {
		l.fail("concurrency %d must be in 1..32", c.Engine.Concurrency)
	}
	if r := c.Engine.MaxRemovalRatio; !(r > 0 && r <= 1) {
		l.fail("maxRemovalRatio %v must be in (0,1]", r)
	}
	// The engine treats 0 as "unset" and substitutes its defaults, so reject 0 here
	// instead of silently running with a different value than configured.
	if c.Engine.MaxRemovalsMin < 1 {
		l.fail("maxRemovalsMin %d must be >= 1", c.Engine.MaxRemovalsMin)
	}
	if c.Engine.PendingMetaTimeout <= 0 {
		l.fail("pendingMetaTimeout %s must be > 0", c.Engine.PendingMetaTimeout)
	}
	if c.Engine.CopyPhaseBudget <= 0 {
		l.fail("copyBudget %s must be > 0", c.Engine.CopyPhaseBudget)
	}
	if host, port, err := net.SplitHostPort(c.HTTPAddr); err != nil || port == "" {
		l.fail("httpAddr %q invalid: want host:port (e.g. \":8080\" or \"127.0.0.1:8080\")", c.HTTPAddr)
	} else if n, perr := strconv.Atoi(port); perr != nil || n < 0 || n > 65535 {
		l.fail("httpAddr %q invalid: port %q must be a number in 0..65535", c.HTTPAddr, port)
	} else {
		_ = host
	}
	if c.Engine.TmpBudgetBytes < 0 {
		l.fail("tmpBudget must be >= 0")
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		l.fail("log format %q invalid (want json|text)", c.LogFormat)
	}
}
