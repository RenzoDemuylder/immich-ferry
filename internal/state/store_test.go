package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func glob(t *testing.T, dir, pat string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "pairs", pat))
	return m
}

func TestRoundTrip(t *testing.T) {
	s, _ := open(t)
	st, status, err := s.Load("p1")
	if err != nil || status != Fresh || !st.Bootstrap || st.Items == nil || st.Hashes == nil || st.Schema != Schema || st.Pair != "p1" {
		t.Fatalf("fresh: %+v %v %v", st, status, err)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	off := 3600
	lat := 1.5
	st.Fingerprint = Fingerprint{UserIDs: [2]string{"u1", "u2"}, AlbumIDs: [2]string{"a", "b"}}
	st.Bootstrap = false
	st.LastSuccess = now
	st.AlbumInfo[1] = &AlbumInfo{Name: "n", Description: "d"}
	it := st.Item("k")
	it.InBoth = true
	it.Refs[0] = []AssetRef{{ID: "1", Checksum: "k"}}
	it.Meta[0] = &Meta{Description: "x", TakenAt: &now, OffsetSec: &off, Lat: &lat, Lon: &lat}
	it.MetaID[0] = "1"
	it.Pending = &Pending{To: 1, TargetID: "9", Since: now}
	st.Hashes["h"] = HashEntry{FileModifiedAt: now, Key: "k"}
	if err := s.Save("p1", st); err != nil {
		t.Fatal(err)
	}
	got, status, err := s.Load("p1")
	if err != nil || status != Loaded {
		t.Fatal(status, err)
	}
	if got.Fingerprint != st.Fingerprint || got.Bootstrap || !got.LastSuccess.Equal(now) || got.AlbumInfo[1].Name != "n" || got.AlbumInfo[0] != nil {
		t.Fatalf("%+v", got)
	}
	gi := got.Items["k"]
	if !gi.InBoth || gi.Refs[0][0].ID != "1" || !gi.Meta[0].TakenAt.Equal(now) || *gi.Meta[0].OffsetSec != 3600 || gi.Pending.TargetID != "9" || gi.Meta[1] != nil {
		t.Fatalf("%+v", gi)
	}
	if got.Hashes["h"].Key != "k" {
		t.Fatal("hash")
	}
	if got.InBothCount() != 1 {
		t.Fatal("count")
	}
	got.Delete("k")
	if got.InBothCount() != 0 {
		t.Fatal("delete")
	}
}

func TestLeftoverTmpIgnored(t *testing.T) {
	s, dir := open(t)
	st := NewPairState("p")
	st.Item("a")
	if err := s.Save("p", st); err != nil {
		t.Fatal(err)
	}
	// Simulated crash mid-save: garbage tmp file.
	if err := os.WriteFile(filepath.Join(dir, "pairs", "p.json.tmp-deadbeef"), []byte("{garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, status, err := s.Load("p")
	if err != nil || status != Loaded || got.Items["a"] == nil {
		t.Fatal(status, err)
	}
	if len(glob(t, dir, "*.json.tmp-*")) != 1 {
		t.Fatal("tmp touched")
	}
	if err := s.Save("p", got); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptAndSchema(t *testing.T) {
	for name, content := range map[string]string{"corrupt": "{not json", "schema": `{"schema":99,"pair":"p"}`} {
		t.Run(name, func(t *testing.T) {
			s, dir := open(t)
			if err := os.WriteFile(filepath.Join(dir, "pairs", "p.json"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			st, status, err := s.Load("p")
			if err != nil || status != CorruptMovedAside || !st.Bootstrap {
				t.Fatal(status, err)
			}
			if len(glob(t, dir, "p.json.corrupt-*")) != 1 {
				t.Fatal("no aside file")
			}
			if _, err := os.Stat(filepath.Join(dir, "pairs", "p.json")); !os.IsNotExist(err) {
				t.Fatal("original still there")
			}
		})
	}
}

func TestLockExclusive(t *testing.T) {
	s, dir := open(t)
	_, err := Open(dir)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	if !strings.Contains(err.Error(), ".lock") {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
}

func TestReadOnly(t *testing.T) {
	_, dir := open(t)
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.Save("p", NewPairState("p")); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	if err := ro.Reset("p"); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	if err := ro.PutMarker("p", "approve-removals"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ro.Load("p"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("want error")
	}
}

func TestMarkers(t *testing.T) {
	s, dir := open(t)
	ok, err := s.TakeMarker("p", "reset")
	if ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := s.PutMarker("p", "reset"); err != nil {
		t.Fatal(err)
	}
	if len(glob(t, dir, "p.reset.marker")) != 1 {
		t.Fatal("marker file")
	}
	for i := 0; i < 2; i++ { // HasMarker does not consume
		if ok, err := s.HasMarker("p", "reset"); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	ok, err = s.TakeMarker("p", "reset")
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := s.HasMarker("p", "reset"); ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "pairs", "p.approve-removals.marker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasMarker("p", "approve-removals"); ok || err == nil {
		t.Fatalf("directory at marker path: ok=%v err=%v, want error", ok, err)
	}
	ok, _ = s.TakeMarker("p", "reset")
	if ok {
		t.Fatal("marker should be consumed")
	}
}

func TestReconcile(t *testing.T) {
	s, dir := open(t)
	fp1 := Fingerprint{UserIDs: [2]string{"a", "b"}, AlbumIDs: [2]string{"x", "y"}}
	fp2 := Fingerprint{UserIDs: [2]string{"a", "b"}, AlbumIDs: [2]string{"x", "z"}}
	st := NewPairState("p")
	st.Bootstrap = false
	got, status, err := s.Reconcile("p", st, fp1)
	if err != nil || status != Loaded || got != st || got.Fingerprint != fp1 {
		t.Fatal(status, err)
	}
	if err := s.Save("p", got); err != nil {
		t.Fatal(err)
	}
	got, status, _ = s.Reconcile("p", got, fp1)
	if status != Loaded || got.Bootstrap {
		t.Fatal("equal case")
	}
	got, status, err = s.Reconcile("p", got, fp2)
	if err != nil || status != FingerprintReset || !got.Bootstrap || got.Fingerprint != fp2 {
		t.Fatal(status, err)
	}
	if len(glob(t, dir, "p.json.fpreset-*")) != 1 {
		t.Fatal("aside")
	}
}

func TestReset(t *testing.T) {
	s, dir := open(t)
	if err := s.Reset("p"); err != nil {
		t.Fatal(err)
	}
	_ = s.Save("p", NewPairState("p"))
	if err := s.Reset("p"); err != nil {
		t.Fatal(err)
	}
	if len(glob(t, dir, "p.json.reset-*")) != 1 || len(glob(t, dir, "p.json")) != 0 {
		t.Fatal("reset")
	}
}

func TestPairNameValidation(t *testing.T) {
	s, _ := open(t)
	for _, n := range []string{"", "A", "-a", "a/b", "../x", "a.b", "a_b", strings.Repeat("a", 64)} {
		if _, _, err := s.Load(n); err == nil {
			t.Errorf("Load %q", n)
		}
		if err := s.Save(n, NewPairState("x")); err == nil {
			t.Errorf("Save %q", n)
		}
		if err := s.Reset(n); err == nil {
			t.Errorf("Reset %q", n)
		}
		if err := s.PutMarker(n, "m"); err == nil {
			t.Errorf("PutMarker %q", n)
		}
		if _, err := s.TakeMarker(n, "m"); err == nil {
			t.Errorf("TakeMarker %q", n)
		}
	}
	if _, _, err := s.Load(strings.Repeat("a", 63)); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSave(t *testing.T) {
	s, _ := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := NewPairState("p")
			for j := 0; j < 20; j++ {
				if err := s.Save("p", st); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if _, status, err := s.Load("p"); err != nil || status != Loaded {
		t.Fatal(status, err)
	}
}

func TestPeekDoesNotMoveCorruptAside(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(dir, "pairs", "p.json")
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, status, err := s.Peek("p")
	if err != nil || status != CorruptMovedAside || st == nil {
		t.Fatalf("%v %v %v", st, status, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Peek moved the file: %v", err)
	}
}

func TestIntentBackwardCompatibleJSON(t *testing.T) {
	var it Item
	if err := json.Unmarshal([]byte(`{"r":[null,null],"m":[null,null],"mi":["",""],"intent":{"to":1,"started":"2025-01-01T00:00:00Z","source":{"id":"a","checksum":"c"},"sourceMeta":{}}}`), &it); err != nil {
		t.Fatal(err)
	}
	if it.Intent == nil || it.Intent.UploadStarted {
		t.Fatalf("%+v", it.Intent)
	}
}
