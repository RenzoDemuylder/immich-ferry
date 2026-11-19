package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DiscoverySchema is the current discovery file schema version.
const DiscoverySchema = 1

// Discovery is the persisted result of album discovery: which album on A is paired with
// which album on B, and which albums must not get a new counterpart. It lives in
// <dir>/discovery.json. Losing it is safe: albums are re-matched by name (union semantics).
type Discovery struct {
	Schema int `json:"schema"`
	// UserIDs are the users the pairs were discovered for; a mismatch discards the file.
	UserIDs    [2]string        `json:"userIds"`
	Pairs      []DiscoveredPair `json:"pairs"`
	Tombstones []Tombstone      `json:"tombstones,omitempty"`
}

// DiscoveredPair pins an album on A to an album on B. Name is the pair name used for
// the pair state file, metrics and CLI commands.
type DiscoveredPair struct {
	Name     string    `json:"name"`
	AlbumIDs [2]string `json:"albumIds"`
	Since    time.Time `json:"since"`
	// Created records that discovery created (and shared) the album on one side. Only
	// such albums are ever torn down when everyone they were shared with has left.
	Created *CreatedAlbum `json:"created,omitempty"`
	// Teardown is the progress of a teardown in progress; such a pair is never synced.
	Teardown *TeardownProgress `json:"teardown,omitempty"`
}

// CreatedAlbum is the album discovery created as a counterpart on Side, shared with
// the users in SharedWith (user ids, never the sync user itself).
type CreatedAlbum struct {
	Side       uint8    `json:"side"`
	SharedWith []string `json:"sharedWith"`
}

// TeardownProgress tracks the steps of a teardown so a failed or interrupted one
// resumes where it stopped.
type TeardownProgress struct {
	Since        time.Time `json:"since"`
	Left         bool      `json:"left,omitempty"`
	AssetsDone   bool      `json:"assetsDone,omitempty"`
	AlbumDeleted bool      `json:"albumDeleted,omitempty"`
	// KeptAlbum: the sync user owns the original album and cannot leave it, so the
	// created album is kept (without members) and blocks a new counterpart.
	KeptAlbum bool `json:"keptAlbum,omitempty"`
}

// Tombstone marks an album whose counterpart was deleted: discovery never creates a new
// counterpart for it (it may still pair with an album of the same name created by a user).
type Tombstone struct {
	Side    uint8     `json:"side"`
	AlbumID string    `json:"albumId"`
	Reason  string    `json:"reason"`
	Since   time.Time `json:"since"`
}

// NewDiscovery returns an empty discovery state.
func NewDiscovery() *Discovery { return &Discovery{Schema: DiscoverySchema} }

func (s *Store) discoveryFile() string { return filepath.Join(s.dir, "discovery.json") }

// LoadDiscovery reads the discovery state. A missing file is Fresh. A corrupt file (or
// another schema) is moved aside on a writable store and reported as CorruptMovedAside;
// a read-only store leaves it in place. A file for other users is discarded the same way
// (FingerprintReset) so pairs never carry over to a different account.
func (s *Store) LoadDiscovery(userIDs [2]string) (*Discovery, LoadStatus, error) {
	path := s.discoveryFile()
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		d := NewDiscovery()
		d.UserIDs = userIDs
		return d, Fresh, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("state: read %s: %w", path, err)
	}
	var d Discovery
	status := Loaded
	suffix := ""
	switch jerr := json.Unmarshal(data, &d); {
	case jerr != nil || d.Schema != DiscoverySchema:
		status, suffix = CorruptMovedAside, "corrupt"
	case d.UserIDs != userIDs:
		status, suffix = FingerprintReset, "fpreset"
	}
	if status == Loaded {
		return &d, Loaded, nil
	}
	if !s.readOnly {
		if err := s.aside(path, suffix); err != nil {
			return nil, 0, fmt.Errorf("state: move aside %s: %w", path, err)
		}
	}
	n := NewDiscovery()
	n.UserIDs = userIDs
	return n, status, nil
}

// PeekDiscovery reads the discovery state without checking users or touching the file
// (for admin commands). A missing or corrupt file yields an empty state.
func (s *Store) PeekDiscovery() (*Discovery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.discoveryFile())
	if errors.Is(err, os.ErrNotExist) {
		return NewDiscovery(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: read discovery: %w", err)
	}
	var d Discovery
	if err := json.Unmarshal(data, &d); err != nil || d.Schema != DiscoverySchema {
		return NewDiscovery(), nil
	}
	return &d, nil
}

// SaveDiscovery atomically writes the discovery state; takes no ctx.
func (s *Store) SaveDiscovery(d *Discovery) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if d == nil {
		return errors.New("state: nil discovery")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state: store closed")
	}
	d.Schema = DiscoverySchema
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("state: marshal discovery: %w", err)
	}
	return writeAtomic(s.discoveryFile(), data)
}

// ReadOnly reports whether the store was opened read-only.
func (s *Store) ReadOnly() bool { return s.readOnly }
