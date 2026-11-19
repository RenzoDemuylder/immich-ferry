package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

// RequiredPermissions returns the sorted, deduplicated API key permissions
// needed for the options.
func RequiredPermissions(o Options) []string {
	set := map[string]bool{}
	for _, p := range []string{"asset.read", "asset.download", "asset.upload", "asset.update",
		"album.read", "albumAsset.create", "user.read"} {
		set[p] = true
	}
	if o.RestoreTrashed {
		set["asset.delete"] = true
	}
	if o.Discovery != DiscoveryOff {
		set["album.create"] = true
	}
	if len(o.DiscoveryShare[A]) > 0 || len(o.DiscoveryShare[B]) > 0 {
		// Teardown when everyone a created album was shared with has left it: delete
		// that album and leave its counterpart.
		set["album.delete"] = true
		set["albumUser.delete"] = true
		if o.DiscoveryTeardownTrash {
			set["asset.delete"] = true
		}
	}
	if o.SyncAlbumInfo {
		set["album.update"] = true
	}
	if o.PropagateRemovals {
		set["albumAsset.delete"] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Engine syncs album pairs.
type Engine struct {
	o   Options
	cl  [2]Immich
	st  *state.Store
	log *slog.Logger

	mu      sync.Mutex
	userIDs [2]string
	pfDone  bool
	// resolved maps a resolved (side, albumID) to the pair that owns it.
	resolved map[resolvedAlbum]string
}

type resolvedAlbum struct {
	side Side
	id   string
}

// cleanTmpDir removes leftover temp files of ours (a crash or kill during a
// transfer) so they cannot fill the temp volume. Only files matching our own
// pattern are touched.
func cleanTmpDir(dir string, log *slog.Logger) {
	matches, err := filepath.Glob(filepath.Join(dir, tmpPattern))
	if err != nil {
		return
	}
	for _, m := range matches {
		if fi, err := os.Lstat(m); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(m); err == nil {
			log.Info("removed leftover temp file", "op", "tmp_cleanup", "file", filepath.Base(m))
		}
	}
}

// New creates an Engine. Zero option values take the config defaults.
func New(o Options, a, b Immich, st *state.Store, log *slog.Logger) (*Engine, error) {
	if a == nil || b == nil {
		return nil, fmt.Errorf("%w: both immich clients are required", ErrConfig)
	}
	if st == nil {
		return nil, fmt.Errorf("%w: state store is required", ErrConfig)
	}
	if o.Concurrency < 0 || o.TmpBudgetBytes < 0 || o.MaxRemovalRatio < 0 || o.MaxRemovalsMin < 0 ||
		o.CheckpointEvery < 0 || o.PendingMetaTimeout < 0 || o.CopyPhaseBudget < 0 || o.CheckpointInterval < 0 ||
		o.BackoffBase < 0 || o.BackoffMax < 0 || o.BlockedRecheck < 0 {
		return nil, fmt.Errorf("%w: negative option value", ErrConfig)
	}
	if o.ConflictWinner != A && o.ConflictWinner != B {
		return nil, fmt.Errorf("%w: invalid conflict winner %d", ErrConfig, o.ConflictWinner)
	}
	if o.Concurrency == 0 {
		o.Concurrency = 4
	}
	if o.TmpDir == "" {
		o.TmpDir = os.TempDir()
	}
	if o.MaxRemovalRatio == 0 {
		o.MaxRemovalRatio = 0.2
	}
	if o.MaxRemovalsMin == 0 {
		o.MaxRemovalsMin = 10
	}
	if o.PendingMetaTimeout == 0 {
		o.PendingMetaTimeout = time.Hour
	}
	if o.CopyPhaseBudget == 0 {
		o.CopyPhaseBudget = 20 * time.Minute
	}
	if o.CheckpointEvery == 0 {
		o.CheckpointEvery = 100
	}
	if o.CheckpointInterval == 0 {
		o.CheckpointInterval = 30 * time.Second
	}
	if o.BackoffBase == 0 {
		o.BackoffBase = 15 * time.Minute
	}
	if o.BackoffMax == 0 {
		o.BackoffMax = 24 * time.Hour
	}
	if o.BlockedRecheck == 0 {
		o.BlockedRecheck = 24 * time.Hour
	}
	if o.BackoffMax < o.BackoffBase {
		o.BackoffMax = o.BackoffBase
	}
	switch o.DiscoveryShareRole {
	case "":
		o.DiscoveryShareRole = "editor"
	case "editor", "viewer":
	default:
		return nil, fmt.Errorf("%w: invalid discovery share role %q (want editor or viewer)", ErrConfig, o.DiscoveryShareRole)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cleanTmpDir(o.TmpDir, log)
	return &Engine{o: o, cl: [2]Immich{a, b}, st: st, log: log, resolved: map[resolvedAlbum]string{}}, nil
}

// Preflight checks versions, API key permissions and user ids on both sides.
func (e *Engine) Preflight(ctx context.Context) error {
	req := RequiredPermissions(e.o)
	var ids [2]string
	for _, s := range []Side{A, B} {
		c := e.cl[s]
		v, err := c.Version(ctx)
		if err != nil {
			return &PreflightError{Side: s, Err: err}
		}
		if v.Major != 3 || v.Minor < 2 {
			return &PreflightError{Side: s, Version: v, Err: fmt.Errorf("%w: unsupported immich version %d.%d.%d (need 3.x with x >= 2)",
				ErrConfig, v.Major, v.Minor, v.Patch)}
		}
		perms, err := c.APIKeyPermissions(ctx)
		if err != nil {
			return &PreflightError{Side: s, Version: v, Err: err}
		}
		if missing := missingPerms(req, perms); len(missing) > 0 {
			return &PreflightError{Side: s, Version: v, Missing: missing,
				Err: fmt.Errorf("%w: API key lacks required permissions", ErrConfig)}
		}
		id, err := c.MyUserID(ctx)
		if err != nil {
			return &PreflightError{Side: s, Version: v, Err: err}
		}
		ids[s] = id
		e.log.Debug("preflight ok", "phase", "preflight", "side", s.String(), "version", fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch))
	}
	e.mu.Lock()
	e.userIDs = ids
	e.pfDone = true
	e.mu.Unlock()
	return nil
}

func missingPerms(required, have []string) []string {
	hs := map[string]bool{}
	for _, p := range have {
		if p == "all" {
			return nil
		}
		hs[p] = true
	}
	var missing []string
	for _, p := range required {
		if !hs[p] {
			missing = append(missing, p)
		}
	}
	return missing
}

var _ = immich.VisibilityTimeline
