package auth

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// The API token gives read-only access to the status API (docs/api.md).
// There is at most one. Only its SHA-256 is stored, in the settings table;
// the token itself is shown once, when it is created.
const (
	settingAPIToken = "api_token"
	// APITokenPrefix starts every token so it is recognisable in configs
	// and by secret scanners.
	APITokenPrefix = "pbt_"
)

// ErrBadAPIToken is returned for a missing, malformed or wrong token.
var ErrBadAPIToken = errors.New("invalid or missing API token")

// APIToken describes the current token (never the token itself).
type APIToken struct {
	CreatedAt time.Time
	LastUsed  time.Time // zero if never used; updated at most every few minutes
}

type apiTokenRecord struct {
	Hash      string `json:"sha256"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used,omitempty"`
}

// APITokenInfo returns the current token's metadata; ok is false if there
// is none.
func (a *Auth) APITokenInfo(ctx context.Context) (APIToken, bool, error) {
	rec, ok, err := a.loadAPIToken(ctx)
	if err != nil || !ok {
		return APIToken{}, false, err
	}
	t := APIToken{CreatedAt: time.Unix(rec.CreatedAt, 0)}
	if rec.LastUsed > 0 {
		t.LastUsed = time.Unix(rec.LastUsed, 0)
	}
	return t, true, nil
}

// RotateAPIToken checks the admin's current password, then creates a new
// token, replacing (and so revoking) the previous one. The returned token is
// not stored anywhere and cannot be shown again.
func (a *Auth) RotateAPIToken(ctx context.Context, password, ip string) (string, error) {
	if err := a.verifyAdminPassword(ctx, password, ip); err != nil {
		return "", err
	}
	tok := APITokenPrefix + randToken()
	rec := apiTokenRecord{Hash: hex.EncodeToString(hashToken(tok)), CreatedAt: time.Now().Unix()}
	a.apiMu.Lock()
	defer a.apiMu.Unlock()
	if err := a.saveAPIToken(ctx, rec); err != nil {
		return "", err
	}
	a.apiTouched = time.Time{}
	return tok, nil
}

// RevokeAPIToken deletes the token; API requests fail from then on.
func (a *Auth) RevokeAPIToken(ctx context.Context) error {
	a.apiMu.Lock()
	defer a.apiMu.Unlock()
	_, err := a.db.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, settingAPIToken)
	return err
}

// CheckAPIToken verifies a bearer token. Failures count towards the same
// per-IP sign-in rate limit as wrong passwords, so a rate-limited IP is
// refused even with the right token.
func (a *Auth) CheckAPIToken(ctx context.Context, tok, ip string) error {
	if err := a.allow(ip); err != nil {
		return err
	}
	rec, ok, err := a.loadAPIToken(ctx)
	if err != nil {
		return err
	}
	want, _ := hex.DecodeString(rec.Hash)
	if !ok || !strings.HasPrefix(tok, APITokenPrefix) || len(want) == 0 ||
		subtle.ConstantTimeCompare(hashToken(tok), want) != 1 {
		a.fail(ip)
		return ErrBadAPIToken
	}
	a.touchAPIToken(ctx, rec.Hash)
	return nil
}

// touchAPIToken records the last use, at most every touchEvery, and only if
// the token was not rotated meanwhile.
func (a *Auth) touchAPIToken(ctx context.Context, hash string) {
	a.apiMu.Lock()
	defer a.apiMu.Unlock()
	now := time.Now()
	if now.Sub(a.apiTouched) < touchEvery {
		return
	}
	rec, ok, err := a.loadAPIToken(ctx)
	if err != nil || !ok || rec.Hash != hash {
		return
	}
	rec.LastUsed = now.Unix()
	if a.saveAPIToken(ctx, rec) == nil {
		a.apiTouched = now
	}
}

func (a *Auth) loadAPIToken(ctx context.Context) (apiTokenRecord, bool, error) {
	var rec apiTokenRecord
	b, ok, err := a.db.GetSetting(ctx, settingAPIToken)
	if err != nil || !ok {
		return rec, false, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, false, err
	}
	return rec, true, nil
}

func (a *Auth) saveAPIToken(ctx context.Context, rec apiTokenRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return a.db.SetSetting(ctx, settingAPIToken, b)
}

// verifyAdminPassword checks password against the admin's, with the
// sign-in rate limit.
func (a *Auth) verifyAdminPassword(ctx context.Context, password, ip string) error {
	if err := a.allow(ip); err != nil {
		return err
	}
	var hash string
	if err := a.db.Reader().QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = 1`).Scan(&hash); err != nil {
		return err
	}
	if !VerifyPassword(password, hash) {
		a.fail(ip)
		return errors.New("current password is wrong")
	}
	return nil
}
