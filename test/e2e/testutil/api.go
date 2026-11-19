package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// API is a tiny raw Immich client. Authenticate with Key (x-api-key) or, for admin-only
// setup calls, Token (bearer).
type API struct {
	URL   string
	Key   string
	Token string
	HTTP  *http.Client
}

func (a *API) hdr() map[string]string {
	h := map[string]string{}
	if a.Key != "" {
		h["x-api-key"] = a.Key
	}
	if a.Token != "" {
		h["Authorization"] = "Bearer " + a.Token
	}
	return h
}

// Do performs a JSON request against /api+path and decodes into out (may be nil).
func (a *API) Do(ctx context.Context, method, path string, body, out any) error {
	_, _, err := do(ctx, a.HTTP, a.URL, method, path, a.hdr(), body, out)
	return err
}

// Raw performs a JSON request and returns status and raw body; err is *HTTPError for non-2xx.
func (a *API) Raw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	return do(ctx, a.HTTP, a.URL, method, path, a.hdr(), body, nil)
}

// Exif is the exifInfo subset used by tests.
type Exif struct {
	Description      *string    `json:"description"`
	DateTimeOriginal *time.Time `json:"dateTimeOriginal"`
	TimeZone         *string    `json:"timeZone"`
	Latitude         *float64   `json:"latitude"`
	Longitude        *float64   `json:"longitude"`
	Rating           *int       `json:"rating"`
	FileSizeInByte   *int64     `json:"fileSizeInByte"`
}

// Asset is the AssetResponseDto subset used by tests.
type Asset struct {
	ID               string    `json:"id"`
	OwnerID          string    `json:"ownerId"`
	Checksum         string    `json:"checksum"`
	LibraryID        *string   `json:"libraryId"`
	OriginalFileName string    `json:"originalFileName"`
	FileCreatedAt    time.Time `json:"fileCreatedAt"`
	FileModifiedAt   time.Time `json:"fileModifiedAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	Type             string    `json:"type"`
	Duration         any       `json:"duration"`
	LivePhotoVideoID *string   `json:"livePhotoVideoId"`
	Visibility       string    `json:"visibility"`
	IsTrashed        bool      `json:"isTrashed"`
	IsFavorite       bool      `json:"isFavorite"`
	IsOffline        bool      `json:"isOffline"`
	Exif             *Exif     `json:"exifInfo"`
}

// UploadOpts are optional upload fields.
type UploadOpts struct {
	FileCreatedAt    time.Time // default: now (UTC)
	FileCreatedRaw   string    // if set, sent verbatim instead of FileCreatedAt (e.g. naive datetime)
	LivePhotoVideoID string
	Visibility       string
	IsFavorite       *bool
}

// Upload sends POST /assets as multipart, text fields first.
func (a *API) Upload(ctx context.Context, filename string, data []byte, o UploadOpts) (id string, duplicate bool, err error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	created := o.FileCreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	cs := created.Format(time.RFC3339)
	if o.FileCreatedRaw != "" {
		cs = o.FileCreatedRaw
	}
	fields := [][2]string{{"fileCreatedAt", cs}, {"fileModifiedAt", created.Format(time.RFC3339)}, {"filename", filename}}
	if o.IsFavorite != nil {
		fields = append(fields, [2]string{"isFavorite", fmt.Sprint(*o.IsFavorite)})
	}
	if o.Visibility != "" {
		fields = append(fields, [2]string{"visibility", o.Visibility})
	}
	if o.LivePhotoVideoID != "" {
		fields = append(fields, [2]string{"livePhotoVideoId", o.LivePhotoVideoID})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return "", false, err
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="assetData"; filename=%q`, filename))
	h.Set("Content-Type", "application/octet-stream")
	pw, err := mw.CreatePart(h)
	if err != nil {
		return "", false, err
	}
	if _, err := pw.Write(data); err != nil {
		return "", false, err
	}
	if err := mw.Close(); err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.URL, "/")+"/api/assets", &buf)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	for k, v := range a.hdr() {
		req.Header.Set(k, v)
	}
	hc := a.HTTP
	if hc == nil {
		hc = defaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", false, &HTTPError{Method: "POST", Path: "/assets", Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", false, fmt.Errorf("upload decode: %w (%s)", err, body)
	}
	// Status code is the authority: 201 created, 200 duplicate.
	return out.ID, resp.StatusCode == http.StatusOK || out.Status == "duplicate", nil
}

// UploadStatus is like Upload but also returns the HTTP status code (0 on transport error).
func (a *API) UploadStatus(ctx context.Context, filename string, data []byte, o UploadOpts) (id string, status int, err error) {
	id, dup, err := a.Upload(ctx, filename, data, o)
	if err != nil {
		return "", StatusOf(err), err
	}
	if dup {
		return id, http.StatusOK, nil
	}
	return id, http.StatusCreated, nil
}

// GetAsset returns the raw JSON object of GET /assets/{id}.
func (a *API) GetAsset(ctx context.Context, id string) (map[string]any, error) {
	var m map[string]any
	err := a.Do(ctx, http.MethodGet, "/assets/"+id, nil, &m)
	return m, err
}

// GetAssetTyped returns the typed GET /assets/{id}.
func (a *API) GetAssetTyped(ctx context.Context, id string) (*Asset, error) {
	var as Asset
	if err := a.Do(ctx, http.MethodGet, "/assets/"+id, nil, &as); err != nil {
		return nil, err
	}
	return &as, nil
}

// UpdateAsset performs PUT /assets/{id} and returns the typed read-back.
func (a *API) UpdateAsset(ctx context.Context, id string, fields map[string]any) (*Asset, error) {
	var as Asset
	if err := a.Do(ctx, http.MethodPut, "/assets/"+id, fields, &as); err != nil {
		return nil, err
	}
	return &as, nil
}

// Trash moves assets to the trash (soft delete).
func (a *API) Trash(ctx context.Context, ids ...string) error {
	return a.Do(ctx, http.MethodDelete, "/assets", map[string]any{"ids": ids, "force": false}, nil)
}

// Restore restores trashed assets.
func (a *API) Restore(ctx context.Context, ids ...string) error {
	return a.Do(ctx, http.MethodPost, "/trash/restore/assets", map[string]any{"ids": ids}, nil)
}

// AlbumUser is an entry of albumUsers.
type AlbumUser struct {
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Role string `json:"role"`
}

// Album is the AlbumResponseDto subset used by tests.
type Album struct {
	ID          string      `json:"id"`
	Name        string      `json:"albumName"`
	Description string      `json:"description"`
	AssetCount  int         `json:"assetCount"`
	OwnerID     string      `json:"ownerId"`
	Shared      bool        `json:"shared"`
	UpdatedAt   time.Time   `json:"updatedAt"`
	Users       []AlbumUser `json:"albumUsers"`
}

// CreateAlbum creates an album (optionally with initial assets) and returns its id.
func (a *API) CreateAlbum(ctx context.Context, name, desc string, assetIDs ...string) (string, error) {
	body := map[string]any{"albumName": name, "description": desc}
	if len(assetIDs) > 0 {
		body["assetIds"] = assetIDs
	}
	var al Album
	if err := a.Do(ctx, http.MethodPost, "/albums", body, &al); err != nil {
		return "", err
	}
	return al.ID, nil
}

// ListAlbums returns GET /albums (no filters).
func (a *API) ListAlbums(ctx context.Context) ([]Album, error) {
	var out []Album
	err := a.Do(ctx, http.MethodGet, "/albums", nil, &out)
	return out, err
}

// DeleteAlbum performs DELETE /albums/{id}.
func (a *API) DeleteAlbum(ctx context.Context, id string) error {
	return a.Do(ctx, http.MethodDelete, "/albums/"+id, nil, nil)
}

// GetAlbum returns GET /albums/{id}.
func (a *API) GetAlbum(ctx context.Context, id string) (*Album, error) {
	var al Album
	if err := a.Do(ctx, http.MethodGet, "/albums/"+id, nil, &al); err != nil {
		return nil, err
	}
	return &al, nil
}

// UpdateAlbum performs PATCH /albums/{id}.
func (a *API) UpdateAlbum(ctx context.Context, id string, fields map[string]any) (*Album, error) {
	var al Album
	if err := a.Do(ctx, http.MethodPatch, "/albums/"+id, fields, &al); err != nil {
		return nil, err
	}
	return &al, nil
}

// BulkResult is one entry of an album add/remove response.
type BulkResult struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// AddToAlbum adds assets and returns the per-id results.
func (a *API) AddToAlbum(ctx context.Context, albumID string, ids ...string) ([]BulkResult, error) {
	var out []BulkResult
	err := a.Do(ctx, http.MethodPut, "/albums/"+albumID+"/assets", map[string]any{"ids": ids}, &out)
	return out, err
}

// RemoveFromAlbum removes assets and returns the per-id results.
func (a *API) RemoveFromAlbum(ctx context.Context, albumID string, ids ...string) ([]BulkResult, error) {
	var out []BulkResult
	err := a.Do(ctx, http.MethodDelete, "/albums/"+albumID+"/assets", map[string]any{"ids": ids}, &out)
	return out, err
}

// ShareAlbum shares an album with a user (role editor|viewer).
func (a *API) ShareAlbum(ctx context.Context, albumID, userID, role string) error {
	return a.Do(ctx, http.MethodPut, "/albums/"+albumID+"/users",
		map[string]any{"albumUsers": []map[string]string{{"userId": userID, "role": role}}}, nil)
}

// SetAlbumUserRole changes a member's role (editor|viewer) via PUT /albums/{id}/user/{userId}.
func (a *API) SetAlbumUserRole(ctx context.Context, albumID, userID, role string) error {
	return a.Do(ctx, http.MethodPut, "/albums/"+albumID+"/user/"+userID, map[string]any{"role": role}, nil)
}

// RemoveAlbumUser removes a member from an album via DELETE /albums/{id}/user/{userId}.
func (a *API) RemoveAlbumUser(ctx context.Context, albumID, userID string) error {
	return a.Do(ctx, http.MethodDelete, "/albums/"+albumID+"/user/"+userID, nil, nil)
}

type searchResp struct {
	Assets struct {
		Items      []Asset `json:"items"`
		NextCursor *string `json:"nextCursor"`
	} `json:"assets"`
}

// SearchStructured pages through POST /search/metadata with the structured body (filter/orderBy/...).
// body is mutated (size, cursor). Returns all items.
func (a *API) SearchStructured(ctx context.Context, body map[string]any) ([]Asset, error) {
	if _, ok := body["size"]; !ok {
		body["size"] = 1000
	}
	var all []Asset
	for {
		var r searchResp
		if err := a.Do(ctx, http.MethodPost, "/search/metadata", body, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Assets.Items...)
		if r.Assets.NextCursor == nil || *r.Assets.NextCursor == "" {
			return all, nil
		}
		body["cursor"] = *r.Assets.NextCursor
	}
}

// AlbumAssets lists non-trashed archive+timeline assets of an album (the DESIGN listing query).
func (a *API) AlbumAssets(ctx context.Context, albumID string) ([]Asset, error) {
	return a.SearchStructured(ctx, map[string]any{
		"filter": map[string]any{
			"albumIds":   map[string]any{"any": []string{albumID}},
			"trashedAt":  map[string]any{"eq": nil},
			"visibility": map[string]any{"in": []string{"archive", "timeline"}},
		},
		"orderBy":  map[string]any{"field": "fileCreatedAt", "direction": "asc"},
		"withExif": true,
	})
}

// AlbumAssetIDs returns the ids from AlbumAssets.
func (a *API) AlbumAssetIDs(ctx context.Context, albumID string) ([]string, error) {
	as, err := a.AlbumAssets(ctx, albumID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(as))
	for i := range as {
		ids[i] = as[i].ID
	}
	return ids, nil
}

// AllAssetIDs returns all non-trashed assets of the caller including hidden ones (not locked).
func (a *API) AllAssetIDs(ctx context.Context) ([]string, error) {
	as, err := a.SearchStructured(ctx, map[string]any{
		"filter": map[string]any{
			"trashedAt":  map[string]any{"eq": nil},
			"visibility": map[string]any{"in": []string{"archive", "timeline", "hidden"}},
		},
		"orderBy": map[string]any{"field": "fileCreatedAt", "direction": "asc"},
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(as))
	for i := range as {
		ids[i] = as[i].ID
	}
	return ids, nil
}

// QueuesIdle reports whether all job queues have no active and no waiting jobs.
// It tries GET /queues (v3) and falls back to GET /jobs (legacy). Needs an admin-capable credential.
func (a *API) QueuesIdle(ctx context.Context) (bool, string, error) {
	var raw any
	err := a.Do(ctx, http.MethodGet, "/queues", nil, &raw)
	if err != nil {
		if err2 := a.Do(ctx, http.MethodGet, "/jobs", nil, &raw); err2 != nil {
			return false, "", fmt.Errorf("queues: %v; jobs: %v", err, err2)
		}
	}
	busy := []string{}
	var walk func(name string, v any)
	walk = func(name string, v any) {
		switch t := v.(type) {
		case map[string]any:
			act, okA := num(t["active"])
			wait, okW := num(t["waiting"])
			if okA || okW {
				if act > 0 || wait > 0 {
					busy = append(busy, fmt.Sprintf("%s(active=%v waiting=%v)", name, act, wait))
				}
				return
			}
			for k, c := range t {
				n := k
				if s, ok := t["name"].(string); ok {
					n = s
				}
				walk(n, c)
			}
		case []any:
			for _, c := range t {
				walk(name, c)
			}
		}
	}
	walk("", raw)
	return len(busy) == 0, strings.Join(busy, ","), nil
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}
