package fakeimmich

import (
	"bytes"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"immich-ferry/internal/immich"
)

var videoExts = map[string]bool{".mp4": true, ".mov": true, ".m4v": true, ".avi": true, ".webm": true, ".mkv": true, ".3gp": true}
var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".heif": true, ".webp": true,
	".gif": true, ".tif": true, ".tiff": true, ".dng": true, ".avif": true, ".bmp": true}

func (s *Server) hUpload(w http.ResponseWriter, r *http.Request, u *user) {
	rec, _ := r.Context().Value(recKey).(*RecordedRequest)
	cb := &countBody{r: r.Body}
	r.Body = cb
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, "Validation failed", fieldErr{"body", "expected multipart/form-data"})
		return
	}
	fields := map[string]string{}
	var form []FormField
	var content []byte
	var partName string
	haveFile := false
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, 400, "Validation failed", fieldErr{"body", "bad multipart: " + err.Error()})
			return
		}
		if p.FileName() != "" || p.FormName() == "assetData" {
			b, err := io.ReadAll(p)
			if err != nil {
				writeErr(w, 400, "Validation failed", fieldErr{"body", "bad multipart: " + err.Error()})
				return
			}
			form = append(form, FormField{Name: p.FormName(), FileName: p.FileName(), FileSize: int64(len(b))})
			if p.FormName() == "assetData" {
				content, partName, haveFile = b, p.FileName(), true
			}
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(p, 1<<20))
		form = append(form, FormField{Name: p.FormName(), Value: string(b)})
		fields[p.FormName()] = string(b)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	s.mu.Lock()
	if rec != nil {
		rec.Form = form
		rec.BodyBytes = cb.n
	}
	s.mu.Unlock()

	parseTime := func(k string) (time.Time, bool) {
		v, ok := fields[k]
		if !ok {
			badRequest(w, k, "Invalid input: expected ISO datetime, received undefined")
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			badRequest(w, k, "Invalid ISO datetime (zone offset required)")
			return time.Time{}, false
		}
		return t, true
	}
	created, ok := parseTime("fileCreatedAt")
	if !ok {
		return
	}
	modified, ok := parseTime("fileModifiedAt")
	if !ok {
		return
	}
	if !haveFile {
		badRequest(w, "assetData", "Invalid input: file is required")
		return
	}
	if len(content) == 0 {
		writeErr(w, 400, "Asset data is empty")
		return
	}
	filename := partName
	if v, ok := fields["filename"]; ok && v != "" {
		filename = v
	}
	ext := strings.ToLower(path.Ext(filename))
	if !videoExts[ext] && !imageExts[ext] {
		writeErr(w, 400, "Unsupported file type "+ext)
		return
	}
	typ := "IMAGE"
	if videoExts[ext] {
		typ = "VIDEO"
	}
	var dur *int64
	if v, ok := fields["duration"]; ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			badRequest(w, "duration", "Invalid input: expected non-negative integer")
			return
		}
		dur = &n
	}
	fav := false
	if v, ok := fields["isFavorite"]; ok {
		if v != "true" && v != "false" {
			badRequest(w, "isFavorite", "Invalid input: expected boolean")
			return
		}
		fav = v == "true"
	}
	vis := immich.VisibilityTimeline
	if v, ok := fields["visibility"]; ok && v != "" {
		if !validVisibility(v) {
			badRequest(w, "visibility", "Invalid option")
			return
		}
		vis = immich.Visibility(v)
	}
	liveID := fields["livePhotoVideoId"]
	if _, ok := fields["livePhotoVideoId"]; ok && !uuidRe.MatchString(liveID) {
		badRequest(w, "livePhotoVideoId", "Invalid UUID")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var motion *FakeAsset
	if liveID != "" {
		motion = s.assets[liveID]
		if motion == nil || motion.OwnerID != u.id || motion.Type != "VIDEO" {
			writeErr(w, 400, "Live photo video must be an existing video you own")
			return
		}
	}
	sum := checksumOf(content, "", "")
	// Rule 1: per (owner, checksum) dedupe for non-library assets, including trashed.
	for _, id := range s.order {
		a := s.assets[id]
		if a.OwnerID == u.id && a.LibraryID == "" && a.Checksum == sum {
			writeJSON(w, 200, map[string]any{"id": a.ID, "status": "duplicate"})
			return
		}
	}
	now := s.tick()
	a := &FakeAsset{ID: newUUID(), OwnerID: u.id, Checksum: sum, OriginalFileName: filename,
		FileCreatedAt: created.UTC(), FileModifiedAt: modified.UTC(), Type: typ, DurationMS: dur,
		IsFavorite: fav, Visibility: vis, Content: bytes.Clone(content), UpdatedAt: now, LivePhotoVideoID: liveID}
	s.assets[a.ID] = a
	s.order = append(s.order, a.ID)
	if motion != nil && motion.Visibility == immich.VisibilityTimeline {
		motion.Visibility = immich.VisibilityHidden
		motion.UpdatedAt = s.tick()
	}
	writeJSON(w, 201, map[string]any{"id": a.ID, "status": "created"})
}

type countBody struct {
	r io.ReadCloser
	n int64
}

func (c *countBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
func (c *countBody) Close() error { return c.r.Close() }
