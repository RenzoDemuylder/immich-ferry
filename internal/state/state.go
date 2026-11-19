// Package state persists per-pair sync state as JSON with atomic saves.
package state

import (
	"errors"
	"time"
)

// Schema is the current state file schema version.
const Schema = 1

// ErrLocked is returned by Open when another process holds <dir>/.lock.
var ErrLocked = errors.New("state: directory locked by another process")

// LoadStatus describes how Load obtained the state.
type LoadStatus int

// Load statuses.
const (
	Fresh LoadStatus = iota
	Loaded
	CorruptMovedAside
	FingerprintReset
)

// PairState is the persisted state of one pair. [2] arrays index A=0, B=1.
type PairState struct {
	Schema      int                  `json:"schema"`
	Pair        string               `json:"pair"`
	Fingerprint Fingerprint          `json:"fingerprint"`
	AlbumIDs    [2]string            `json:"albumIds"`
	Bootstrap   bool                 `json:"bootstrap"`
	LastSuccess time.Time            `json:"lastSuccess"`
	AlbumInfo   [2]*AlbumInfo        `json:"albumInfo"`
	Items       map[string]*Item     `json:"items"`
	Hashes      map[string]HashEntry `json:"hashes"`
}

// Fingerprint identifies the instances/albums a state belongs to (no URLs).
type Fingerprint struct {
	UserIDs  [2]string `json:"userIds"`
	AlbumIDs [2]string `json:"albumIds"`
}

// AlbumInfo is the last observed album name/description of a side.
type AlbumInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AssetRef references an asset by id and checksum.
type AssetRef struct {
	ID       string `json:"id"`
	Checksum string `json:"checksum"`
}

// Item is the state of one sync key (checksum).
type Item struct {
	InBoth  bool          `json:"b,omitempty"`
	Refs    [2][]AssetRef `json:"r"`
	Meta    [2]*Meta      `json:"m"`
	MetaID  [2]string     `json:"mi"`
	Intent  *Intent       `json:"intent,omitempty"`
	Pending *Pending      `json:"pending,omitempty"`
	Blocked *Blocked      `json:"blocked,omitempty"`
	Failure *Failure      `json:"failure,omitempty"`
	// Ours holds, per side, the id of the asset this tool uploaded for the key (an
	// upload that returned "created", or our own retried duplicate). Only such assets
	// may ever be trashed (discovery teardown).
	Ours [2]string `json:"o,omitzero"`
}

// Meta is the normalized last-observed mergeable metadata of an asset.
type Meta struct {
	Description string     `json:"description,omitempty"`
	TakenAt     *time.Time `json:"takenAt,omitempty"`
	OffsetSec   *int       `json:"offsetSec,omitempty"`
	Lat         *float64   `json:"lat,omitempty"`
	Lon         *float64   `json:"lon,omitempty"`
	Rating      *int       `json:"rating,omitempty"`
	Visibility  string     `json:"visibility,omitempty"`
}

// Intent is a write-ahead record of a copy in progress.
type Intent struct {
	To         uint8     `json:"to"`
	Started    time.Time `json:"started"`
	Source     AssetRef  `json:"source"`
	SourceMeta Meta      `json:"sourceMeta"`
	// UploadStarted is set (and persisted) right before an upload is sent.
	// Only an intent with this marker proves that a target asset found later
	// is ours. Intents from older versions lack it and are never trusted.
	UploadStarted bool `json:"uploadStarted,omitempty"`
}

// Pending marks an uploaded asset whose metadata extraction is not yet done.
type Pending struct {
	To       uint8     `json:"to"`
	TargetID string    `json:"targetId"`
	Since    time.Time `json:"since"`
}

// Blocked marks a key the planner must leave alone.
type Blocked struct {
	Op     string    `json:"op"`
	Side   uint8     `json:"side"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// Failure records per-key failure backoff.
type Failure struct {
	Op        string    `json:"op"`
	Count     int       `json:"count"`
	Next      time.Time `json:"next"`
	LastError string    `json:"lastError"`
}

// HashEntry caches the content hash of an external-library asset.
type HashEntry struct {
	FileModifiedAt time.Time `json:"fileModifiedAt"`
	Key            string    `json:"key"`
}

// NewPairState returns a fresh state for pair (Bootstrap set, maps initialised).
func NewPairState(pair string) *PairState {
	return &PairState{Schema: Schema, Pair: pair, Bootstrap: true, Items: map[string]*Item{}, Hashes: map[string]HashEntry{}}
}

// Item returns the item for key, creating it if absent.
func (st *PairState) Item(key string) *Item {
	if st.Items == nil {
		st.Items = map[string]*Item{}
	}
	it := st.Items[key]
	if it == nil {
		it = &Item{}
		st.Items[key] = it
	}
	return it
}

// Delete removes the item for key.
func (st *PairState) Delete(key string) { delete(st.Items, key) }

// InBothCount returns |S|: the number of items known to be in both albums.
func (st *PairState) InBothCount() int {
	n := 0
	for _, it := range st.Items {
		if it != nil && it.InBoth {
			n++
		}
	}
	return n
}

func (st *PairState) init() {
	if st.Items == nil {
		st.Items = map[string]*Item{}
	}
	if st.Hashes == nil {
		st.Hashes = map[string]HashEntry{}
	}
}
