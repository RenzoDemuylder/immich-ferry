package testutil

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Admin credentials used for every bootstrapped e2e instance.
const (
	AdminEmail    = "admin@immich-ferry.test"
	AdminPassword = "immich-ferry-e2e"
	AdminName     = "Admin"
)

// MinimalPermissions is the permission set documented for immich-ferry API keys with every option enabled,
// plus album.create, which the smoke test needs to create its own albums (immich-ferry never creates albums).
var MinimalPermissions = []string{
	"asset.read", "asset.download", "asset.upload", "asset.update", "asset.delete",
	"album.read", "album.create", "album.update", "albumAsset.create", "albumAsset.delete",
	"user.read",
}

// Instance is one bootstrapped Immich server.
type Instance struct {
	URL           string
	AdminToken    string // bearer access token of the admin user
	AdminID       string
	AdminEmail    string
	AdminPassword string
}

// WaitReady polls GET /api/server/ping until the server answers.
func WaitReady(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		_, _, err := do(ctx, nil, url, http.MethodGet, "/server/ping", nil, nil, nil)
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("server %s not ready after %s: %w", url, timeout, last)
}

// Login signs in with email/password and returns the access token and user id.
func Login(ctx context.Context, url, email, password string) (token, userID string, err error) {
	var out struct {
		AccessToken string `json:"accessToken"`
		UserID      string `json:"userId"`
	}
	_, _, err = do(ctx, nil, url, http.MethodPost, "/auth/login", nil, map[string]string{"email": email, "password": password}, &out)
	if err != nil {
		return "", "", err
	}
	return out.AccessToken, out.UserID, nil
}

// Bootstrap creates the admin user (if missing), logs in and marks onboarding as done. It is idempotent.
func Bootstrap(ctx context.Context, url string) (*Instance, error) {
	if err := WaitReady(ctx, url, 5*time.Minute); err != nil {
		return nil, err
	}
	_, _, err := do(ctx, nil, url, http.MethodPost, "/auth/admin-sign-up", nil,
		map[string]string{"email": AdminEmail, "password": AdminPassword, "name": AdminName}, nil)
	if err != nil && StatusOf(err) != http.StatusBadRequest { // 400: admin already exists
		return nil, fmt.Errorf("admin-sign-up: %w", err)
	}
	tok, uid, err := Login(ctx, url, AdminEmail, AdminPassword)
	if err != nil {
		return nil, fmt.Errorf("admin login: %w", err)
	}
	in := &Instance{URL: url, AdminToken: tok, AdminID: uid, AdminEmail: AdminEmail, AdminPassword: AdminPassword}
	// Best effort: skip the onboarding wizard.
	_, _, _ = do(ctx, nil, url, http.MethodPost, "/system-metadata/admin-onboarding", in.bearer(tok), map[string]bool{"isOnboarded": true}, nil)
	return in, nil
}

func (in *Instance) bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// CreateAPIKey creates an API key with exactly perms for the user owning token and returns the secret.
func (in *Instance) CreateAPIKey(ctx context.Context, token string, perms []string) (string, error) {
	var out struct {
		Secret string `json:"secret"`
	}
	name := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	_, _, err := do(ctx, nil, in.URL, http.MethodPost, "/api-keys", in.bearer(token),
		map[string]any{"name": name, "permissions": perms}, &out)
	if err != nil {
		return "", err
	}
	if out.Secret == "" {
		return "", fmt.Errorf("api-keys: empty secret")
	}
	return out.Secret, nil
}

// CreateUser creates a (non-admin) user via the admin API and logs in. If the user already exists it only logs in.
func (in *Instance) CreateUser(ctx context.Context, email, password, name string) (id, token string, err error) {
	var out struct {
		ID string `json:"id"`
	}
	_, _, err = do(ctx, nil, in.URL, http.MethodPost, "/admin/users", in.bearer(in.AdminToken),
		map[string]any{"email": email, "password": password, "name": name, "shouldChangePassword": false}, &out)
	if err != nil && StatusOf(err) != http.StatusBadRequest {
		return "", "", fmt.Errorf("create user: %w", err)
	}
	token, uid, lerr := Login(ctx, in.URL, email, password)
	if lerr != nil {
		return "", "", fmt.Errorf("login %s: %w (create err: %v)", email, lerr, err)
	}
	return uid, token, nil
}

// AdminAPI returns an API client authenticated with the admin bearer token.
func (in *Instance) AdminAPI() *API { return &API{URL: in.URL, Token: in.AdminToken} }

// NewUserAPI creates a user and a minimal-permission API key for it and returns the client plus user id.
func (in *Instance) NewUserAPI(ctx context.Context, email, name string, perms []string) (*API, string, error) {
	id, tok, err := in.CreateUser(ctx, email, "user-password-1", name)
	if err != nil {
		return nil, "", err
	}
	key, err := in.CreateAPIKey(ctx, tok, perms)
	if err != nil {
		return nil, "", err
	}
	return &API{URL: in.URL, Key: key}, id, nil
}
