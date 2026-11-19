// Package engine runs one sync cycle for a pair of albums.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"immich-ferry/internal/immich"
)

// Immich is the Immich API surface the engine needs (satisfied by *immich.Client).
type Immich interface {
	Version(ctx context.Context) (immich.Version, error)
	APIKeyPermissions(ctx context.Context) ([]string, error)
	MyUserID(ctx context.Context) (string, error)
	GetAlbum(ctx context.Context, id string) (immich.Album, error)
	ListAlbums(ctx context.Context) ([]immich.Album, error)
	ListUsers(ctx context.Context) ([]immich.User, error)
	CreateAlbum(ctx context.Context, name, desc string, users []immich.AlbumUser) (immich.Album, error)
	UpdateAlbumInfo(ctx context.Context, id string, u immich.AlbumInfoUpdate) (immich.Album, error)
	AddToAlbum(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error)
	RemoveFromAlbum(ctx context.Context, albumID string, ids []string) ([]immich.BulkIDResult, error)
	ListAlbumAssets(ctx context.Context, albumID string) ([]immich.Asset, error)
	ProbeAlbumAssets(ctx context.Context, albumID string, checksums []string) ([]immich.Asset, error)
	GetAsset(ctx context.Context, id string) (immich.Asset, error)
	UpdateAsset(ctx context.Context, id string, u immich.AssetUpdate) (immich.Asset, error)
	UploadCheck(ctx context.Context, items []immich.UploadCheckItem) ([]immich.UploadCheckResult, error)
	DownloadOriginal(ctx context.Context, id string, w io.Writer, onBytes func(int64)) (int64, error)
	Upload(ctx context.Context, r immich.UploadRequest, onBytes func(int64)) (immich.UploadResult, error)
	RestoreFromTrash(ctx context.Context, ids []string) error
	// Discovery teardown only.
	DeleteAlbum(ctx context.Context, id string) error
	LeaveAlbum(ctx context.Context, id string) error
	AlbumsWithAsset(ctx context.Context, assetID string) ([]immich.Album, error)
	TrashAssets(ctx context.Context, ids []string) error
}

// Side identifies one of the two instances.
type Side uint8

// Sides.
const (
	A Side = 0
	B Side = 1
)

// String returns "a" or "b".
func (s Side) String() string {
	if s == A {
		return "a"
	}
	return "b"
}

// AlbumRef identifies an album by its id.
type AlbumRef struct{ ID string }

// Pair is a configured album pair. Name matches ^[a-z0-9][a-z0-9-]{0,62}$.
type Pair struct {
	Name string
	A, B AlbumRef
}

// Options configure the engine.
type Options struct {
	Concurrency       int
	TmpDir            string
	TmpBudgetBytes    int64
	DryRun            bool
	PropagateRemovals bool
	MaxRemovalRatio   float64
	MaxRemovalsMin    int
	ConflictWinner    Side
	SyncMetadata      bool
	SyncAlbumInfo     bool
	RestoreTrashed    bool
	// Discovery selects albums automatically instead of configured pairs (see discover.go).
	Discovery       DiscoveryMode
	DiscoveryMarker string
	// DiscoveryShare lists, per side, the users (id or email) that albums created by
	// discovery on that side are shared with, with role DiscoveryShareRole.
	DiscoveryShare     [2][]string
	DiscoveryShareRole string // editor (default) | viewer
	// DiscoveryTeardownTrash: when everyone an album created by discovery was shared with
	// has left it, the teardown also moves the assets this tool uploaded into it to the
	// trash (only those in no other album the sync user can see). Off by default: a
	// member may have put such assets into an album of their own, which the sync user
	// cannot see.
	DiscoveryTeardownTrash bool
	// DiscoveryDedicatedUser declares that the sync user owns no albums except those
	// discovery created. Then an unpaired album it owns with no other member, on a side
	// with DiscoveryShare, can only be one everyone left: it is never synced and blocks
	// pairing by name, even when discovery.json was lost.
	DiscoveryDedicatedUser bool
	AllowMassRemoval       bool
	PendingMetaTimeout     time.Duration
	CopyPhaseBudget        time.Duration
	CheckpointEvery        int
	CheckpointInterval     time.Duration
	BackoffBase            time.Duration
	BackoffMax             time.Duration
	BlockedRecheck         time.Duration
	Now                    func() time.Time
	OnProgress             func(ProgressEvent)
}

// Phase is a cycle phase.
type Phase string

// Phases.
const (
	PhasePreflight  Phase = "preflight"
	PhaseDiscovery  Phase = "discovery"
	PhaseSnapshot   Phase = "snapshot"
	PhasePlan       Phase = "plan"
	PhaseRemovals   Phase = "removals"
	PhaseMetadata   Phase = "metadata"
	PhaseAlbumInfo  Phase = "albuminfo"
	PhaseCopy       Phase = "copy"
	PhaseCheckpoint Phase = "checkpoint"
	PhaseDone       Phase = "done"
)

// ProgressEvent reports progress within a phase.
type ProgressEvent struct {
	Pair        string
	Phase       Phase
	Done, Total int
	Bytes       int64
}

// Result is the outcome class of a run.
type Result string

// Results.
const (
	ResultOK      Result = "ok"
	ResultPartial Result = "partial"
	ResultFailed  Result = "failed"
	ResultDryRun  Result = "dry_run"
)

// PerDir counts per direction.
type PerDir struct{ AtoB, BtoA int }

// SkipReason explains why an item was skipped.
type SkipReason string

// Skip reasons.
const (
	SkipNotOwner     SkipReason = "not_owner"
	SkipBlocked      SkipReason = "blocked"
	SkipBackoff      SkipReason = "backoff"
	SkipTargetHidden SkipReason = "target_hidden"
	SkipTargetTrash  SkipReason = "target_trashed"
	SkipServerHidden SkipReason = "server_hidden"
	SkipOffline      SkipReason = "offline"
	SkipStale        SkipReason = "stale"
	SkipQuota        SkipReason = "quota"
	SkipTooLarge     SkipReason = "too_large"
	SkipGuard        SkipReason = "guard"
	// SkipSnapshotIncomplete: a side's snapshot lacks assets (hash failures).
	SkipSnapshotIncomplete SkipReason = "snapshot_incomplete"
)

// ItemError is a per-item failure.
type ItemError struct {
	Key, Op string
	Side    Side
	Kind    immich.Kind
	Msg     string
}

// RunSummary is the result of one RunPair.
type RunSummary struct {
	Pair                                                              string
	Result                                                            Result
	Started, Finished                                                 time.Time
	AlbumIDs                                                          [2]string
	AlbumSizes                                                        [2]int
	SnapshotConsistent, GuardTripped                                  [2]bool
	Bootstrap                                                         bool
	Copied, Linked, Restored, Unlinked, MetaUpdates, AlbumInfoUpdated PerDir
	BytesDown, BytesUp                                                int64
	Pending, Deferred                                                 int
	Skipped                                                           map[SkipReason]int
	ItemErrors                                                        []ItemError // capped at 100
	ItemErrorCount                                                    int
	Mutations                                                         int
}

// ErrConfig marks configuration errors (exit code 2).
var ErrConfig = errors.New("configuration error")

// PreflightError describes a failed preflight for one side.
type PreflightError struct {
	Side    Side
	Version immich.Version
	Missing []string
	Err     error
}

// Error implements error.
func (e *PreflightError) Error() string {
	return fmt.Sprintf("preflight failed for side %s: %v (missing permissions: %v)", e.Side, e.Err, e.Missing)
}

// Unwrap returns the underlying error.
func (e *PreflightError) Unwrap() error { return e.Err }
