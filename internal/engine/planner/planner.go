// Package planner is the pure sync planner: truth table, guards, blocked
// semantics and per-side metadata merge. It performs no IO.
package planner

import (
	"time"

	"immich-ferry/internal/immich"
	"immich-ferry/internal/state"
)

// Key is the sync identity of an asset (checksum or content hash).
type Key string

// Member is one asset of a key group on one side.
type Member struct {
	Ref              state.AssetRef
	Owned, External  bool
	Meta             state.Meta
	LivePhotoVideoID string
	// Extracted reports that the server finished metadata extraction
	// (exif dateTimeOriginal != nil).
	Extracted     bool
	Visibility    immich.Visibility
	FileCreatedAt time.Time
}

// Snapshot is the grouped content of one album side.
type Snapshot struct {
	AlbumID    string
	Consistent bool
	Groups     map[Key][]Member // [0] = primary

	AlbumName, AlbumDescription string
}

// ActionKind is the kind of a planned action.
type ActionKind string

// Action kinds.
const (
	ActionCopy        ActionKind = "copy"
	ActionProbeUnlink ActionKind = "probe_unlink"
	ActionMeta        ActionKind = "meta"
	ActionLinkRepair  ActionKind = "link_repair"
	ActionAlbumInfo   ActionKind = "album_info"
	ActionDrop        ActionKind = "drop"
	ActionBlock       ActionKind = "block"
)

// Action is one planned step. From/To are sides (A=0, B=1).
type Action struct {
	Kind     ActionKind
	Key      Key
	From, To uint8
	Refs     []state.AssetRef
	Update   *immich.AssetUpdate
	TargetID string

	// Meta actions: per-side updates, target asset ids, new baselines (and the
	// asset ids they are bound to).
	Updates    [2]*immich.AssetUpdate
	TargetIDs  [2]string
	NewBase    [2]*state.Meta
	NewBaseIDs [2]string
	// ClearPending asks the engine to clear Item.Pending.
	ClearPending bool
	// Album info actions.
	AlbumUpdates [2]*immich.AlbumInfoUpdate
	NewAlbumBase [2]*state.AlbumInfo
	// Reason carries conflict notes (comma separated) or a short explanation.
	Reason string
}

// Plan is the output of Build.
type Plan struct {
	Removals, Meta, AlbumInfo, Copies []Action
	GuardTripped                      [2]bool
	Warnings                          []string

	// Paired lists keys now in both albums (engine sets InBoth, refreshes Refs/MetaID).
	Paired []Key
	// Drops lists keys absent from both albums that were in S.
	Drops []Key
	// Skipped counts skipped keys by reason (engine.SkipReason strings).
	Skipped map[string]int
	// Pending counts paired keys waiting for target metadata extraction.
	Pending int
	// RemovalCandidates counts removal candidates per side to unlink from.
	RemovalCandidates [2]int
}

// Options are the planner-relevant options (mapped by the engine).
type Options struct {
	PropagateRemovals bool
	MaxRemovalRatio   float64
	MaxRemovalsMin    int
	AllowMassRemoval  bool
	ConflictWinner    uint8
	SyncMetadata      bool
	SyncAlbumInfo     bool
	Bootstrap         bool
	BlockedRecheck    time.Duration
	BackoffBase       time.Duration
	BackoffMax        time.Duration
	// PendingMetaTimeout bounds how long a pending upload waits for extraction.
	PendingMetaTimeout time.Duration
}
