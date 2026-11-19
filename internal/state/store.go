package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var pairNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var markerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ErrReadOnly is returned by mutating calls on a read-only Store.
var ErrReadOnly = errors.New("state: store is read-only")

// Store is a state directory handle.
type Store struct {
	dir      string
	readOnly bool
	lock     *os.File
	mu       sync.Mutex
	closed   bool
}

// Open opens dir read-write and takes the <dir>/.lock flock; returns ErrLocked if held.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "pairs"), 0o755); err != nil {
		return nil, fmt.Errorf("state: create dir: %w", err)
	}
	lockPath := filepath.Join(dir, ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("state: open lock %s: %w", lockPath, err)
	}
	if err := flockExclusive(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, lockPath)
		}
		return nil, fmt.Errorf("state: lock %s: %w", lockPath, err)
	}
	// Write probe: fail fast on a read-only directory.
	probe, err := os.CreateTemp(filepath.Join(dir, "pairs"), ".probe-*")
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("state: directory %s not writable: %w", dir, err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return &Store{dir: dir, lock: f}, nil
}

// OpenReadOnly opens dir without locking (for admin subcommands).
func OpenReadOnly(dir string) (*Store, error) {
	if st, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("state: %s is not a directory", dir)
	}
	return &Store{dir: dir, readOnly: true}, nil
}

func (s *Store) pairsDir() string { return filepath.Join(s.dir, "pairs") }

func (s *Store) file(pair string) (string, error) {
	if !pairNameRE.MatchString(pair) {
		return "", fmt.Errorf("state: invalid pair name %q", pair)
	}
	return filepath.Join(s.pairsDir(), pair+".json"), nil
}

func (s *Store) marker(pair, name string) (string, error) {
	if !pairNameRE.MatchString(pair) {
		return "", fmt.Errorf("state: invalid pair name %q", pair)
	}
	if !markerNameRE.MatchString(name) {
		return "", fmt.Errorf("state: invalid marker name %q", name)
	}
	return filepath.Join(s.pairsDir(), pair+"."+name+".marker"), nil
}

func (s *Store) aside(path, suffix string) error {
	return os.Rename(path, fmt.Sprintf("%s.%s-%d", path, suffix, time.Now().UnixNano()))
}

// Load reads the state of pair.
func (s *Store) Load(pair string) (*PairState, LoadStatus, error) {
	path, err := s.file(pair)
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewPairState(pair), Fresh, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("state: read %s: %w", path, err)
	}
	var st PairState
	if jerr := json.Unmarshal(data, &st); jerr != nil || st.Schema != Schema {
		if s.readOnly {
			return NewPairState(pair), CorruptMovedAside, nil
		}
		if err := s.aside(path, "corrupt"); err != nil {
			return nil, 0, fmt.Errorf("state: move aside %s: %w", path, err)
		}
		return NewPairState(pair), CorruptMovedAside, nil
	}
	st.init()
	return &st, Loaded, nil
}

// Peek is Load without any side effect on disk: a corrupt or new-schema file
// is reported as CorruptMovedAside with a fresh state but is left in place
// (used by dry runs and plan-only calls).
func (s *Store) Peek(pair string) (*PairState, LoadStatus, error) {
	path, err := s.file(pair)
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewPairState(pair), Fresh, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("state: read %s: %w", path, err)
	}
	var st PairState
	if jerr := json.Unmarshal(data, &st); jerr != nil || st.Schema != Schema {
		return NewPairState(pair), CorruptMovedAside, nil
	}
	st.init()
	return &st, Loaded, nil
}

// Save atomically writes the state of pair (tmp+fsync+rename+dir fsync); takes no ctx.
func (s *Store) Save(pair string, st *PairState) error {
	path, err := s.file(pair)
	if err != nil {
		return err
	}
	if s.readOnly {
		return ErrReadOnly
	}
	if st == nil {
		return errors.New("state: nil state")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state: store closed")
	}
	if st.Schema == 0 {
		st.Schema = Schema
	}
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	return writeAtomic(path, data)
}

// writeAtomic writes data to path via tmp file + fsync + rename + dir fsync.
func writeAtomic(path string, data []byte) error {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := path + ".tmp-" + hex.EncodeToString(rnd[:])
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("state: create tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("state: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("state: fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: rename: %w", err)
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("state: open dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("state: fsync dir: %w", err)
	}
	return nil
}

// Reconcile compares st.Fingerprint with fp. A zero stored fingerprint adopts
// fp; equal returns st unchanged; different moves the current file aside and
// returns a fresh Bootstrap state carrying fp with status FingerprintReset.
func (s *Store) Reconcile(pair string, st *PairState, fp Fingerprint) (*PairState, LoadStatus, error) {
	path, err := s.file(pair)
	if err != nil {
		return nil, 0, err
	}
	if st.Fingerprint == (Fingerprint{}) {
		st.Fingerprint = fp
		return st, Loaded, nil
	}
	if st.Fingerprint == fp {
		return st, Loaded, nil
	}
	if s.readOnly {
		return nil, 0, ErrReadOnly
	}
	s.mu.Lock()
	err = s.aside(path, "fpreset")
	s.mu.Unlock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, fmt.Errorf("state: move aside %s: %w", path, err)
	}
	n := NewPairState(pair)
	n.Fingerprint = fp
	return n, FingerprintReset, nil
}

// Reset moves the state of pair aside (".reset-<unixnano>") if it exists.
func (s *Store) Reset(pair string) error {
	path, err := s.file(pair)
	if err != nil {
		return err
	}
	if s.readOnly {
		return ErrReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.aside(path, "reset"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state: reset %s: %w", path, err)
	}
	return nil
}

// PutMarker creates a one-shot marker (lock-free; allowed on read-only stores).
func (s *Store) PutMarker(pair, name string) error {
	path, err := s.marker(pair, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.pairsDir(), 0o755); err != nil {
		return fmt.Errorf("state: marker: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o644); err != nil {
		return fmt.Errorf("state: marker: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: marker: %w", err)
	}
	return nil
}

// HasMarker reports whether a marker exists without consuming it. Anything other
// than a regular file at the marker path is an error.
func (s *Store) HasMarker(pair, name string) (bool, error) {
	path, err := s.marker(pair, name)
	if err != nil {
		return false, err
	}
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("state: marker: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("state: marker %s is not a regular file", path)
	}
	return true, nil
}

// TakeMarker consumes a marker and reports whether it existed.
func (s *Store) TakeMarker(pair, name string) (bool, error) {
	path, err := s.marker(pair, name)
	if err != nil {
		return false, err
	}
	err = os.Remove(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("state: marker: %w", err)
}

// Close releases the lock.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.lock != nil {
		err := s.lock.Close() // closing releases the flock
		s.lock = nil
		return err
	}
	return nil
}
