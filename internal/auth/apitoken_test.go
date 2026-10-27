package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAPIToken(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	a := New(st)
	const pw = "correct horse battery"
	if err := a.SetAdmin(ctx, "me@example.com", pw); err != nil {
		t.Fatal(err)
	}

	// No token: everything is refused.
	if err := a.CheckAPIToken(ctx, APITokenPrefix+"x", "192.0.2.1"); !errors.Is(err, ErrBadAPIToken) {
		t.Fatalf("no token configured: %v", err)
	}
	if _, ok, _ := a.APITokenInfo(ctx); ok {
		t.Fatal("token info without a token")
	}

	// Creating one needs the current password.
	if _, err := a.RotateAPIToken(ctx, "wrong password!!", "192.0.2.2"); err == nil {
		t.Fatal("rotated with a wrong password")
	}
	tok1, err := a.RotateAPIToken(ctx, pw, "192.0.2.2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok1, APITokenPrefix) || len(tok1) < 40 {
		t.Fatalf("token %q", tok1)
	}
	// Only the hash is stored.
	raw, _, _ := st.GetSetting(ctx, settingAPIToken)
	if strings.Contains(string(raw), tok1[len(APITokenPrefix):]) {
		t.Fatal("token stored in clear")
	}
	if err := a.CheckAPIToken(ctx, tok1, "192.0.2.3"); err != nil {
		t.Fatalf("valid token refused: %v", err)
	}
	info, ok, _ := a.APITokenInfo(ctx)
	if !ok || info.LastUsed.IsZero() || time.Since(info.CreatedAt) > time.Minute {
		t.Fatalf("info %+v %v", info, ok)
	}
	for _, bad := range []string{"", tok1 + "x", tok1[:len(tok1)-1], strings.TrimPrefix(tok1, APITokenPrefix)} {
		if err := a.CheckAPIToken(ctx, bad, "192.0.2.4"); !errors.Is(err, ErrBadAPIToken) {
			t.Errorf("accepted %q: %v", bad, err)
		}
	}

	// Rotating revokes the old token.
	tok2, err := a.RotateAPIToken(ctx, pw, "192.0.2.2")
	if err != nil {
		t.Fatal(err)
	}
	if tok2 == tok1 {
		t.Fatal("rotation returned the same token")
	}
	if err := a.CheckAPIToken(ctx, tok1, "192.0.2.5"); !errors.Is(err, ErrBadAPIToken) {
		t.Fatalf("old token still works: %v", err)
	}
	if err := a.CheckAPIToken(ctx, tok2, "192.0.2.5"); err != nil {
		t.Fatalf("new token refused: %v", err)
	}

	// Revoking stops it.
	if err := a.RevokeAPIToken(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckAPIToken(ctx, tok2, "192.0.2.6"); !errors.Is(err, ErrBadAPIToken) {
		t.Fatalf("revoked token works: %v", err)
	}
}

// Wrong tokens share the sign-in rate limit: after 5 failures from an IP,
// even the right token is refused there, while other IPs still work.
func TestAPITokenRateLimit(t *testing.T) {
	ctx := context.Background()
	a := New(openStore(t))
	const pw = "correct horse battery"
	if err := a.SetAdmin(ctx, "me@example.com", pw); err != nil {
		t.Fatal(err)
	}
	tok, err := a.RotateAPIToken(ctx, pw, "198.51.100.1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < failsPerIP; i++ {
		a.CheckAPIToken(ctx, APITokenPrefix+"guess", "203.0.113.9")
	}
	if err := a.CheckAPIToken(ctx, tok, "203.0.113.9"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate-limited IP got %v", err)
	}
	if err := a.CheckAPIToken(ctx, tok, "203.0.113.10"); err != nil {
		t.Fatalf("other IP refused: %v", err)
	}
}
