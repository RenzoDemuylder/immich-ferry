package immich

import (
	"context"
	"net/url"
	"time"
)

// Version returns the server version (GET /server/version).
func (c *Client) Version(ctx context.Context) (Version, error) {
	var r struct{ Major, Minor, Patch int }
	if err := c.doJSON(ctx, "GET", "/server/version", nil, nil, &r); err != nil {
		return Version{}, err
	}
	return Version{Major: r.Major, Minor: r.Minor, Patch: r.Patch}, nil
}

// APIKeyPermissions returns the permissions of the API key (GET /api-keys/me).
func (c *Client) APIKeyPermissions(ctx context.Context) ([]string, error) {
	var r struct {
		Permissions []string `json:"permissions"`
	}
	if err := c.doJSON(ctx, "GET", "/api-keys/me", nil, nil, &r); err != nil {
		return nil, err
	}
	return r.Permissions, nil
}

// MyUserID returns the API key owner's user id (GET /users/me).
func (c *Client) MyUserID(ctx context.Context) (string, error) {
	var r struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, "GET", "/users/me", nil, nil, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

type albumDTO struct {
	ID          string    `json:"id"`
	AlbumName   string    `json:"albumName"`
	Description string    `json:"description"`
	AssetCount  int       `json:"assetCount"`
	UpdatedAt   time.Time `json:"updatedAt"`
	AlbumUsers  []struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Role string `json:"role"`
	} `json:"albumUsers"`
}

func (d albumDTO) album() Album {
	a := Album{ID: d.ID, Name: d.AlbumName, Description: d.Description, AssetCount: d.AssetCount, UpdatedAt: d.UpdatedAt}
	for _, u := range d.AlbumUsers {
		a.Users = append(a.Users, AlbumUser{UserID: u.User.ID, Role: u.Role})
	}
	return a
}

// GetAlbum fetches album info without assets.
func (c *Client) GetAlbum(ctx context.Context, id string) (Album, error) {
	var d albumDTO
	if err := c.doJSON(ctx, "GET", "/albums/"+url.PathEscape(id), nil, nil, &d); err != nil {
		return Album{}, err
	}
	return d.album(), nil
}

// ListAlbums lists every album the caller can access. Verified against v3.4.1: with
// neither isOwned nor isShared set, GET /albums returns owned albums and albums shared
// with the caller (any role); albumUsers carries the roles.
func (c *Client) ListAlbums(ctx context.Context) ([]Album, error) {
	var ds []albumDTO
	if err := c.doJSON(ctx, "GET", "/albums", nil, nil, &ds); err != nil {
		return nil, err
	}
	out := make([]Album, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.album())
	}
	return out, nil
}

// ListUsers lists the users visible to the caller (GET /users). Verified against v3.4.1:
// with the default "public users" server setting it returns every user.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var ds []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := c.doJSON(ctx, "GET", "/users", nil, nil, &ds); err != nil {
		return nil, err
	}
	out := make([]User, 0, len(ds))
	for _, d := range ds {
		out = append(out, User{ID: d.ID, Email: d.Email, Name: d.Name})
	}
	return out, nil
}

type albumUserCreate struct {
	UserID string `json:"userId"`
	Role   string `json:"role"`
}

type albumCreate struct {
	AlbumName   string            `json:"albumName"`
	Description string            `json:"description"`
	AlbumUsers  []albumUserCreate `json:"albumUsers,omitempty"`
}

// CreateAlbum creates an album, shared with users (role editor|viewer) in the same
// request, so the album never exists unshared. It is deliberately NOT retried: POST
// /albums is not idempotent, so a retry after an ambiguous failure (5xx/timeout after
// the request may have reached the server) could create a duplicate album. Album
// discovery adopts an album created by an ambiguous failure in its next cycle (it
// matches by name first). Verified against v3.4.1: needs only album.create; an unknown
// user id is a 400 and creates nothing; the caller's own id is ignored.
func (c *Client) CreateAlbum(ctx context.Context, name, desc string, users []AlbumUser) (Album, error) {
	body := albumCreate{AlbumName: name, Description: desc}
	for _, u := range users {
		body.AlbumUsers = append(body.AlbumUsers, albumUserCreate{UserID: u.UserID, Role: u.Role})
	}
	var d albumDTO
	if err := c.doJSONOnce(ctx, "POST", "/albums", body, &d); err != nil {
		return Album{}, err
	}
	return d.album(), nil
}

// UpdateAlbumInfo patches album name/description.
func (c *Client) UpdateAlbumInfo(ctx context.Context, id string, u AlbumInfoUpdate) (Album, error) {
	var d albumDTO
	if err := c.doJSON(ctx, "PATCH", "/albums/"+url.PathEscape(id), nil, u, &d); err != nil {
		return Album{}, err
	}
	return d.album(), nil
}

const idChunk = 1000

type bulkIDsBody struct {
	IDs []string `json:"ids"`
}

type bulkResultDTO struct {
	ID           string `json:"id"`
	Success      bool   `json:"success"`
	Error        string `json:"error"`
	ErrorMessage string `json:"errorMessage"`
}

func chunks[T any](s []T, n int) [][]T {
	var out [][]T
	for len(s) > 0 {
		k := min(n, len(s))
		out = append(out, s[:k])
		s = s[k:]
	}
	return out
}

func (c *Client) albumBulk(ctx context.Context, method, albumID string, ids []string) ([]BulkIDResult, error) {
	var res []BulkIDResult
	for _, ch := range chunks(ids, idChunk) {
		var rs []bulkResultDTO
		if err := c.doJSON(ctx, method, "/albums/"+url.PathEscape(albumID)+"/assets", nil, bulkIDsBody{IDs: ch}, &rs); err != nil {
			return res, err
		}
		for _, r := range rs {
			res = append(res, BulkIDResult{ID: r.ID, Success: r.Success, Error: r.Error})
		}
	}
	return res, nil
}

// AddToAlbum adds assets to an album (chunked).
func (c *Client) AddToAlbum(ctx context.Context, albumID string, ids []string) ([]BulkIDResult, error) {
	return c.albumBulk(ctx, "PUT", albumID, ids)
}

// RemoveFromAlbum unlinks assets from an album (chunked); never deletes assets.
func (c *Client) RemoveFromAlbum(ctx context.Context, albumID string, ids []string) ([]BulkIDResult, error) {
	return c.albumBulk(ctx, "DELETE", albumID, ids)
}

// DeleteAlbum deletes an album (DELETE /albums/{id}). Verified against v3.4.1: the
// album's assets are kept; a gone album is 400 "Not found or no album.delete access"
// (KindNotFound).
func (c *Client) DeleteAlbum(ctx context.Context, id string) error {
	return c.doJSON(ctx, "DELETE", "/albums/"+url.PathEscape(id), nil, nil, nil)
}

// LeaveAlbum removes the caller from an album shared with it (DELETE
// /albums/{id}/user/me, permission albumUser.delete). Verified against v3.4.1: the
// owner cannot leave (400 "Cannot remove the last album owner"); leaving again is 400
// "Album not shared with user"; afterwards the album is not accessible.
func (c *Client) LeaveAlbum(ctx context.Context, id string) error {
	return c.doJSON(ctx, "DELETE", "/albums/"+url.PathEscape(id)+"/user/me", nil, nil, nil)
}

// AlbumsWithAsset lists the albums the caller can access (owned or shared with it) that
// contain the asset (GET /albums?assetId=). Verified against v3.4.1: other users'
// unshared albums are never listed, even when they contain the caller's asset.
func (c *Client) AlbumsWithAsset(ctx context.Context, assetID string) ([]Album, error) {
	var ds []albumDTO
	if err := c.doJSON(ctx, "GET", "/albums", url.Values{"assetId": {assetID}}, nil, &ds); err != nil {
		return nil, err
	}
	out := make([]Album, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.album())
	}
	return out, nil
}

// TrashAssets moves assets to the trash (DELETE /assets with force=false). There is
// deliberately no way to delete permanently.
func (c *Client) TrashAssets(ctx context.Context, ids []string) error {
	type body struct {
		IDs   []string `json:"ids"`
		Force bool     `json:"force"`
	}
	for _, ch := range chunks(ids, idChunk) {
		if err := c.doJSON(ctx, "DELETE", "/assets", nil, body{IDs: ch, Force: false}, nil); err != nil {
			return err
		}
	}
	return nil
}

// RestoreFromTrash restores trashed assets.
func (c *Client) RestoreFromTrash(ctx context.Context, ids []string) error {
	for _, ch := range chunks(ids, idChunk) {
		if err := c.doJSON(ctx, "POST", "/trash/restore/assets", nil, bulkIDsBody{IDs: ch}, nil); err != nil {
			return err
		}
	}
	return nil
}
