package immich

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type exifDTO struct {
	Description      *string    `json:"description"`
	DateTimeOriginal *time.Time `json:"dateTimeOriginal"`
	TimeZone         *string    `json:"timeZone"`
	Latitude         *float64   `json:"latitude"`
	Longitude        *float64   `json:"longitude"`
	Rating           *int       `json:"rating"`
	FileSizeInByte   *int64     `json:"fileSizeInByte"`
}

type assetDTO struct {
	ID               string          `json:"id"`
	OwnerID          string          `json:"ownerId"`
	Checksum         string          `json:"checksum"`
	LibraryID        *string         `json:"libraryId"`
	OriginalFileName string          `json:"originalFileName"`
	FileCreatedAt    time.Time       `json:"fileCreatedAt"`
	FileModifiedAt   time.Time       `json:"fileModifiedAt"`
	Type             string          `json:"type"`
	Duration         json.RawMessage `json:"duration"`
	LivePhotoVideoID *string         `json:"livePhotoVideoId"`
	IsFavorite       bool            `json:"isFavorite"`
	IsTrashed        bool            `json:"isTrashed"`
	IsOffline        bool            `json:"isOffline"`
	Visibility       Visibility      `json:"visibility"`
	ExifInfo         *exifDTO        `json:"exifInfo"`
}

// parseDuration accepts int milliseconds (v3) or a legacy "H:MM:SS.ffffff" string.
func parseDuration(raw json.RawMessage) *int64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		v := int64(n)
		return &v
	}
	var str string
	if json.Unmarshal(raw, &str) != nil {
		return nil
	}
	parts := strings.Split(str, ":")
	if len(parts) != 3 {
		return nil
	}
	h, e1 := strconv.ParseFloat(parts[0], 64)
	m, e2 := strconv.ParseFloat(parts[1], 64)
	sec, e3 := strconv.ParseFloat(parts[2], 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return nil
	}
	v := int64((h*3600 + m*60 + sec) * 1000)
	return &v
}

func (d assetDTO) asset() Asset {
	a := Asset{ID: d.ID, OwnerID: d.OwnerID, Checksum: d.Checksum, LibraryID: d.LibraryID,
		OriginalFileName: d.OriginalFileName, FileCreatedAt: d.FileCreatedAt, FileModifiedAt: d.FileModifiedAt,
		Type: d.Type, DurationMS: parseDuration(d.Duration), LivePhotoVideoID: d.LivePhotoVideoID,
		IsFavorite: d.IsFavorite, IsTrashed: d.IsTrashed, IsOffline: d.IsOffline, Visibility: d.Visibility}
	if d.ExifInfo != nil {
		e := Exif(*d.ExifInfo)
		a.Exif = &e
	}
	return a
}

// GetAsset fetches one asset.
func (c *Client) GetAsset(ctx context.Context, id string) (Asset, error) {
	var d assetDTO
	if err := c.doJSON(ctx, "GET", "/assets/"+url.PathEscape(id), nil, nil, &d); err != nil {
		return Asset{}, err
	}
	return d.asset(), nil
}

// UpdateAsset PUTs the Set fields of u and returns the read-back asset.
func (c *Client) UpdateAsset(ctx context.Context, id string, u AssetUpdate) (Asset, error) {
	var d assetDTO
	if err := c.doJSON(ctx, "PUT", "/assets/"+url.PathEscape(id), nil, u, &d); err != nil {
		return Asset{}, err
	}
	return d.asset(), nil
}

// UploadCheck runs bulk-upload-check (chunked).
func (c *Client) UploadCheck(ctx context.Context, items []UploadCheckItem) ([]UploadCheckResult, error) {
	type item struct {
		ID       string `json:"id"`
		Checksum string `json:"checksum"`
	}
	var out []UploadCheckResult
	for _, ch := range chunks(items, idChunk) {
		body := struct {
			Assets []item `json:"assets"`
		}{}
		for _, it := range ch {
			body.Assets = append(body.Assets, item(it))
		}
		var resp struct {
			Results []struct {
				ID        string `json:"id"`
				Action    string `json:"action"`
				Reason    string `json:"reason"`
				AssetID   string `json:"assetId"`
				IsTrashed bool   `json:"isTrashed"`
			} `json:"results"`
		}
		if err := c.doJSON(ctx, "POST", "/assets/bulk-upload-check", nil, body, &resp); err != nil {
			return out, err
		}
		for _, r := range resp.Results {
			out = append(out, UploadCheckResult{ID: r.ID, Accept: r.Action == "accept", Reason: r.Reason, AssetID: r.AssetID, IsTrashed: r.IsTrashed})
		}
	}
	return out, nil
}

// ---- search ----

type idsFilter struct {
	Any []string `json:"any"`
}
type eqNull struct {
	Eq *string `json:"eq"`
}
type visIn struct {
	In []string `json:"in"`
}
type strIn struct {
	In []string `json:"in"`
}
type searchFilter struct {
	AlbumIDs   idsFilter `json:"albumIds"`
	Checksum   *strIn    `json:"checksum,omitempty"`
	TrashedAt  *eqNull   `json:"trashedAt,omitempty"`
	Visibility *visIn    `json:"visibility,omitempty"`
}
type searchOrder struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}
type searchBody struct {
	Filter   searchFilter `json:"filter"`
	OrderBy  searchOrder  `json:"orderBy"`
	WithExif bool         `json:"withExif"`
	Size     int          `json:"size"`
	Cursor   string       `json:"cursor,omitempty"`
}

const (
	searchPageSize = 1000
	probeChunk     = 500
)

// searchAll pages through POST /search/metadata until the cursor is exhausted,
// deduplicating by id (first occurrence wins).
func (c *Client) searchAll(ctx context.Context, f searchFilter, seen map[string]bool, out *[]Asset) error {
	body := searchBody{Filter: f, OrderBy: searchOrder{"fileCreatedAt", "asc"}, WithExif: true, Size: searchPageSize}
	for {
		var resp struct {
			Assets struct {
				Items      []assetDTO `json:"items"`
				NextCursor *string    `json:"nextCursor"`
			} `json:"assets"`
		}
		if err := c.doJSON(ctx, "POST", "/search/metadata", nil, body, &resp); err != nil {
			return err
		}
		for _, d := range resp.Assets.Items {
			if seen[d.ID] {
				continue
			}
			seen[d.ID] = true
			*out = append(*out, d.asset())
		}
		next := ""
		if resp.Assets.NextCursor != nil {
			next = *resp.Assets.NextCursor
		}
		if next == "" {
			return nil
		}
		if next == body.Cursor {
			return fmt.Errorf("immich POST /search/metadata: cursor did not advance (%q)", next)
		}
		body.Cursor = next
	}
}

// ListAlbumAssets lists all visible (timeline+archive, non-trashed) album assets via structured search.
func (c *Client) ListAlbumAssets(ctx context.Context, albumID string) ([]Asset, error) {
	f := searchFilter{
		AlbumIDs:   idsFilter{Any: []string{albumID}},
		TrashedAt:  &eqNull{},
		Visibility: &visIn{In: []string{"archive", "timeline"}},
	}
	var out []Asset
	if err := c.searchAll(ctx, f, map[string]bool{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ProbeAlbumAssets looks up album members by checksum including trashed and hidden ones.
func (c *Client) ProbeAlbumAssets(ctx context.Context, albumID string, checksums []string) ([]Asset, error) {
	var out []Asset
	seen := map[string]bool{}
	for _, ch := range chunks(checksums, probeChunk) {
		f := searchFilter{AlbumIDs: idsFilter{Any: []string{albumID}}, Checksum: &strIn{In: ch}}
		if err := c.searchAll(ctx, f, seen, &out); err != nil {
			return out, err
		}
	}
	return out, nil
}
