package testutil

import (
	"bytes"
	"encoding/json"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestJPEGUniqueAndDecodable(t *testing.T) {
	a, b := JPEG(JPEGOpts{Seed: 1}), JPEG(JPEGOpts{Seed: 2})
	if SHA1Base64(a) == SHA1Base64(b) {
		t.Fatal("different seeds must yield different checksums")
	}
	if SHA1Base64(a) != SHA1Base64(JPEG(JPEGOpts{Seed: 1})) {
		t.Fatal("same seed must be deterministic")
	}
	d := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	lat, lon := 48.8584, -2.2945
	j := JPEG(JPEGOpts{Seed: 3, DateTimeOriginal: &d, OffsetTime: "+02:00", Lat: &lat, Lon: &lon})
	if _, err := jpeg.Decode(bytes.NewReader(j)); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestMediaUnique(t *testing.T) {
	if SHA1Base64(MP4(1)) == SHA1Base64(MP4(2)) || SHA1Base64(MOV(1)) == SHA1Base64(MOV(2)) {
		t.Fatal("seeded videos must differ")
	}
	if len(tinyMP4) == 0 || len(tinyMP4) > 30000 || len(tinyMOV) == 0 || len(tinyMOV) > 30000 {
		t.Fatalf("fixture sizes: %d %d", len(tinyMP4), len(tinyMOV))
	}
}

func TestJPEGExifWithExiftool(t *testing.T) {
	et, err := exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool not installed")
	}
	d := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	lat, lon := 48.8584, -2.2945
	p := filepath.Join(t.TempDir(), "x.jpg")
	if err := os.WriteFile(p, JPEG(JPEGOpts{Seed: 9, DateTimeOriginal: &d, OffsetTime: "+02:00", Lat: &lat, Lon: &lon}), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(et, "-j", "-n", "-DateTimeOriginal", "-OffsetTimeOriginal", "-GPSLatitude", "-GPSLongitude", "-GPSLatitudeRef", "-GPSLongitudeRef", p).CombinedOutput()
	if err != nil {
		t.Fatalf("exiftool: %v: %s", err, out)
	}
	var res []map[string]any
	if err := json.Unmarshal(out, &res); err != nil || len(res) != 1 {
		t.Fatalf("parse: %v: %s", err, out)
	}
	m := res[0]
	if m["DateTimeOriginal"] != "2021:05:06 07:08:09" {
		t.Errorf("DateTimeOriginal = %v", m["DateTimeOriginal"])
	}
	if m["OffsetTimeOriginal"] != "+02:00" {
		t.Errorf("OffsetTimeOriginal = %v", m["OffsetTimeOriginal"])
	}
	if v, _ := m["GPSLatitude"].(float64); math.Abs(v-lat) > 1e-5 {
		t.Errorf("GPSLatitude = %v want %v", m["GPSLatitude"], lat)
	}
	if v, _ := m["GPSLongitude"].(float64); math.Abs(v+lon) > 1e-5 && math.Abs(v-lon) > 1e-5 {
		t.Errorf("GPSLongitude = %v want %v", m["GPSLongitude"], lon)
	}
}
