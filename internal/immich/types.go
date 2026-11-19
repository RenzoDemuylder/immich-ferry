// Package immich is a typed REST client for the Immich API (v3.x).
package immich

import (
	"encoding/json"
	"os"
	"time"
)

// Visibility is an asset visibility: timeline|archive|hidden|locked.
type Visibility string

// Known visibilities.
const (
	VisibilityTimeline Visibility = "timeline"
	VisibilityArchive  Visibility = "archive"
	VisibilityHidden   Visibility = "hidden"
	VisibilityLocked   Visibility = "locked"
)

// Version is the server version.
type Version struct{ Major, Minor, Patch int }

// AlbumUser is a user with a role on an album.
type AlbumUser struct{ UserID, Role string }

// User is a user as listed by GET /users.
type User struct{ ID, Email, Name string }

// Album is an album (without assets).
type Album struct {
	ID, Name, Description string
	AssetCount            int
	UpdatedAt             time.Time
	Users                 []AlbumUser
}

// Exif is the used subset of asset EXIF info.
type Exif struct {
	Description      *string
	DateTimeOriginal *time.Time
	TimeZone         *string
	Latitude         *float64
	Longitude        *float64
	Rating           *int
	FileSizeInByte   *int64
}

// Asset is the used subset of AssetResponseDto.
type Asset struct {
	ID, OwnerID, Checksum string
	LibraryID             *string
	OriginalFileName      string
	FileCreatedAt         time.Time
	FileModifiedAt        time.Time
	Type                  string
	DurationMS            *int64
	LivePhotoVideoID      *string
	IsFavorite            bool
	IsTrashed             bool
	IsOffline             bool
	Visibility            Visibility
	Exif                  *Exif
}

// Opt is a tri-state JSON field: !Set omits the field, Set&&Val==nil marshals
// as JSON null, Set&&Val!=nil marshals the value.
// Structs containing Opt fields need a custom MarshalJSON (implemented in the
// client package) that skips unset fields; use optJSON as the helper.
type Opt[T any] struct {
	Set bool
	Val *T
}

// Some returns an Opt that is set to v.
func Some[T any](v T) Opt[T] { return Opt[T]{Set: true, Val: &v} }

// Null returns an Opt that is set to JSON null.
func Null[T any]() Opt[T] { return Opt[T]{Set: true} }

// optJSON returns the raw JSON for a set Opt value (helper for MarshalJSON
// implementations of AssetUpdate / AlbumInfoUpdate).
func optJSON[T any](o Opt[T]) (json.RawMessage, error) {
	if o.Val == nil {
		return json.RawMessage("null"), nil
	}
	return json.Marshal(*o.Val)
}

// AssetUpdate is a partial PUT /assets/{id} body; only Set fields are sent.
type AssetUpdate struct {
	Description, DateTimeOriginal Opt[string]
	Latitude, Longitude           Opt[float64]
	Rating                        Opt[int]
	IsFavorite                    Opt[bool]
	Visibility                    Opt[Visibility]
	LivePhotoVideoID              Opt[string]
}

// AlbumInfoUpdate is a partial PATCH /albums/{id} body.
type AlbumInfoUpdate struct{ Name, Description Opt[string] }

// BulkIDResult is one entry of a bulk album/asset operation response.
type BulkIDResult struct {
	ID      string
	Success bool
	Error   string
}

// UploadCheckItem is one asset for bulk-upload-check.
type UploadCheckItem struct{ ID, Checksum string }

// UploadCheckResult is one bulk-upload-check result.
type UploadCheckResult struct {
	ID        string
	Accept    bool
	Reason    string
	AssetID   string
	IsTrashed bool
}

// UploadRequest describes an asset upload; File must be positioned at start.
type UploadRequest struct {
	Filename         string
	FileCreatedAt    time.Time
	FileModifiedAt   time.Time
	DurationMS       *int64
	IsFavorite       *bool
	Visibility       Visibility
	LivePhotoVideoID string
	File             *os.File
	// Checksum is the optional base64 SHA1 of File. When set and an attempt fails after
	// the whole body was sent (response timeout, 5xx), the client asks bulk-upload-check
	// before retrying and returns the existing asset (Duplicate=true) instead of re-uploading.
	Checksum string
}

// UploadResult is the upload response.
type UploadResult struct {
	ID        string
	Duplicate bool
}

var _ = optJSON[string]
