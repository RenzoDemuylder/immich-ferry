package testutil

import (
	"bytes"
	"crypto/sha1"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand"
	"sort"
	"time"
)

//go:embed testdata/tiny.mp4
var tinyMP4 []byte

//go:embed testdata/tiny.mov
var tinyMOV []byte

// SHA1Base64 returns the base64 SHA-1 of data (Immich's checksum format).
func SHA1Base64(data []byte) string {
	s := sha1.Sum(data)
	return base64.StdEncoding.EncodeToString(s[:])
}

// MP4 returns the tiny MP4 fixture plus a seeded trailing `free` box so the checksum is unique.
func MP4(seed int64) []byte { return withFree(tinyMP4, seed) }

// MOV returns the tiny MOV fixture plus a seeded trailing `free` box so the checksum is unique.
func MOV(seed int64) []byte { return withFree(tinyMOV, seed) }

func withFree(base []byte, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	box := make([]byte, 24)
	binary.BigEndian.PutUint32(box[0:4], 24)
	copy(box[4:8], "free")
	for i := 8; i < 24; i++ {
		box[i] = byte(r.Intn(256))
	}
	out := make([]byte, 0, len(base)+24)
	out = append(out, base...)
	return append(out, box...)
}

// JPEGOpts controls JPEG generation.
type JPEGOpts struct {
	Seed             int64
	DateTimeOriginal *time.Time // wall-clock fields of the given time are written as-is
	OffsetTime       string     // e.g. "+02:00" (OffsetTimeOriginal), only with DateTimeOriginal
	Lat, Lon         *float64
	Width, Height    int // default 128x96
}

// JPEG returns a JPEG of seeded random noise (unique checksum per seed) with a hand-written EXIF APP1 segment.
func JPEG(o JPEGOpts) []byte {
	w, h := o.Width, o.Height
	if w <= 0 {
		w = 128
	}
	if h <= 0 {
		h = 96
	}
	r := rand.New(rand.NewSource(o.Seed))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		panic(err)
	}
	raw := buf.Bytes()
	tiff := buildTIFF(o)
	if tiff == nil {
		return raw
	}
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:4], uint16(2+6+len(tiff)))
	seg = append(seg, "Exif\x00\x00"...)
	seg = append(seg, tiff...)
	out := make([]byte, 0, len(raw)+len(seg))
	out = append(out, raw[:2]...) // SOI
	out = append(out, seg...)
	return append(out, raw[2:]...)
}

type ifdEntry struct {
	tag   uint16
	typ   uint16 // 2 ASCII, 4 LONG, 5 RATIONAL
	count uint32
	data  []byte // already little-endian encoded; for pointer entries filled later
}

func ifdTableSize(es []ifdEntry) int { return 2 + 12*len(es) + 4 }

func ifdTotalSize(es []ifdEntry) int {
	n := ifdTableSize(es)
	for _, e := range es {
		if len(e.data) > 4 {
			n += len(e.data) + len(e.data)%2
		}
	}
	return n
}

func encodeIFD(es []ifdEntry, off int) []byte {
	sort.Slice(es, func(i, j int) bool { return es[i].tag < es[j].tag })
	out := make([]byte, ifdTableSize(es))
	binary.LittleEndian.PutUint16(out, uint16(len(es)))
	var extra []byte
	extraOff := off + ifdTableSize(es)
	for i, e := range es {
		p := 2 + 12*i
		binary.LittleEndian.PutUint16(out[p:], e.tag)
		binary.LittleEndian.PutUint16(out[p+2:], e.typ)
		binary.LittleEndian.PutUint32(out[p+4:], e.count)
		if len(e.data) <= 4 {
			copy(out[p+8:p+12], e.data)
		} else {
			binary.LittleEndian.PutUint32(out[p+8:], uint32(extraOff+len(extra)))
			extra = append(extra, e.data...)
			if len(e.data)%2 == 1 {
				extra = append(extra, 0)
			}
		}
	}
	return append(out, extra...)
}

func ascii(tag uint16, s string) ifdEntry {
	b := append([]byte(s), 0)
	return ifdEntry{tag: tag, typ: 2, count: uint32(len(b)), data: b}
}

func long(tag uint16, v uint32) ifdEntry {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return ifdEntry{tag: tag, typ: 4, count: 1, data: b}
}

func dms(tag uint16, deg float64) ifdEntry {
	deg = math.Abs(deg)
	d := math.Floor(deg)
	m := math.Floor((deg - d) * 60)
	s := (deg - d - m/60) * 3600
	const den = 1000000
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b[0:], uint32(d))
	binary.LittleEndian.PutUint32(b[4:], 1)
	binary.LittleEndian.PutUint32(b[8:], uint32(m))
	binary.LittleEndian.PutUint32(b[12:], 1)
	binary.LittleEndian.PutUint32(b[16:], uint32(math.Round(s*den)))
	binary.LittleEndian.PutUint32(b[20:], den)
	return ifdEntry{tag: tag, typ: 5, count: 3, data: b}
}

func buildTIFF(o JPEGOpts) []byte {
	hasDate := o.DateTimeOriginal != nil
	hasGPS := o.Lat != nil && o.Lon != nil
	if !hasDate && !hasGPS {
		return nil
	}
	var exifES, gpsES []ifdEntry
	if hasDate {
		exifES = append(exifES, ifdEntry{tag: 0x9000, typ: 7, count: 4, data: []byte("0232")})
		exifES = append(exifES, ascii(0x9003, o.DateTimeOriginal.Format("2006:01:02 15:04:05")))
		if o.OffsetTime != "" {
			exifES = append(exifES, ascii(0x9011, o.OffsetTime))
		}
	}
	if hasGPS {
		latRef, lonRef := "N", "E"
		if *o.Lat < 0 {
			latRef = "S"
		}
		if *o.Lon < 0 {
			lonRef = "W"
		}
		gpsES = []ifdEntry{ascii(0x0001, latRef), dms(0x0002, *o.Lat), ascii(0x0003, lonRef), dms(0x0004, *o.Lon)}
	}
	// IFD0 has up to two pointer entries; compute layout.
	n0 := 0
	if len(exifES) > 0 {
		n0++
	}
	if len(gpsES) > 0 {
		n0++
	}
	ifd0Size := 2 + 12*n0 + 4
	exifOff := 8 + ifd0Size
	gpsOff := exifOff
	if len(exifES) > 0 {
		gpsOff += ifdTotalSize(exifES)
	}
	var ifd0 []ifdEntry
	if len(exifES) > 0 {
		ifd0 = append(ifd0, long(0x8769, uint32(exifOff)))
	}
	if len(gpsES) > 0 {
		ifd0 = append(ifd0, long(0x8825, uint32(gpsOff)))
	}
	out := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	out = append(out, encodeIFD(ifd0, 8)...)
	if len(exifES) > 0 {
		out = append(out, encodeIFD(exifES, exifOff)...)
	}
	if len(gpsES) > 0 {
		out = append(out, encodeIFD(gpsES, gpsOff)...)
	}
	want := gpsOff
	if len(gpsES) > 0 {
		want += ifdTotalSize(gpsES)
	}
	if len(out) != want {
		panic(fmt.Sprintf("exif layout mismatch: %d", len(out)))
	}
	return out
}
