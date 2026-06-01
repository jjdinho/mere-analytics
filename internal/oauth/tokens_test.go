package oauth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/oauth"
)

func TestIssueAccessToken_LookupActive_HappyPath(t *testing.T) {
	oauthSvc, authSvc, _ := fixture(t)
	userID, projectID := seedUserAndProject(t, authSvc, "alice@example.com")
	client := registerClient(t, oauthSvc, "http://localhost:9999/cb")
	ctx := context.Background()

	plaintext, expiresAt, err := oauthSvc.IssueAccessToken(ctx, oauth.IssueAccessTokenParams{
		ClientID:  client.ID,
		UserID:    userID,
		ProjectID: projectID,
		Scope:     oauth.ScopeAPI,
	})
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}
	if len(plaintext) != 43 {
		t.Errorf("plaintext length: got %d want 43", len(plaintext))
	}
	if time.Until(expiresAt) < 30*time.Minute {
		t.Errorf("expiry too soon: %v", expiresAt)
	}

	got, err := oauthSvc.LookupActiveAccessToken(ctx, plaintext)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got == nil {
		t.Fatalf("lookup returned nil for valid token")
	}
	if got.UserID != userID || got.ProjectID != projectID || got.ClientID != client.ID {
		t.Errorf("identity mismatch: %+v", got)
	}
}

func TestLookupActiveAccessToken_RejectsPublicPrefix(t *testing.T) {
	oauthSvc, _, _ := fixture(t)
	ac, err := oauthSvc.LookupActiveAccessToken(context.Background(), auth.PublicTokenPrefix+strings.Repeat("a", 43))
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ac != nil {
		t.Errorf("public-prefixed token should be rejected pre-DB; got %+v", ac)
	}
}

func TestLookupActiveAccessToken_Garbage_Empty_ReturnsNil(t *testing.T) {
	oauthSvc, _, _ := fixture(t)
	ctx := context.Background()
	for _, in := range []string{"", "not-a-token", strings.Repeat("z", 200)} {
		ac, err := oauthSvc.LookupActiveAccessToken(ctx, in)
		if err != nil {
			t.Errorf("lookup(%q): %v", in, err)
		}
		if ac != nil {
			t.Errorf("lookup(%q): got %+v want nil", in, ac)
		}
	}
}

func TestLookupActiveAccessToken_ExpiredAndRevoked(t *testing.T) {
	oauthSvc, authSvc, pool := fixture(t)
	userID, projectID := seedUserAndProject(t, authSvc, "alice@example.com")
	client := registerClient(t, oauthSvc, "http://localhost:9999/cb")
	ctx := context.Background()

	// Back-dated issuance → already-expired token.
	oauthSvc.SetNow(func() time.Time { return time.Now().Add(-2 * time.Hour) })
	expired, _, err := oauthSvc.IssueAccessToken(ctx, oauth.IssueAccessTokenParams{
		ClientID: client.ID, UserID: userID, ProjectID: projectID, Scope: oauth.ScopeAPI,
	})
	if err != nil {
		t.Fatalf("issue expired: %v", err)
	}
	oauthSvc.SetNow(time.Now)

	got, err := oauthSvc.LookupActiveAccessToken(ctx, expired)
	if err != nil {
		t.Fatalf("lookup expired: %v", err)
	}
	if got != nil {
		t.Errorf("expired token should not lookup; got %+v", got)
	}

	// Fresh token, then forcibly revoke via SQL.
	live, _, err := oauthSvc.IssueAccessToken(ctx, oauth.IssueAccessTokenParams{
		ClientID: client.ID, UserID: userID, ProjectID: projectID, Scope: oauth.ScopeAPI,
	})
	if err != nil {
		t.Fatalf("issue live: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE oauth_access_tokens SET revoked_at = NOW()`); err != nil {
		t.Fatalf("force revoke: %v", err)
	}
	got, err = oauthSvc.LookupActiveAccessToken(ctx, live)
	if err != nil {
		t.Fatalf("lookup revoked: %v", err)
	}
	if got != nil {
		t.Errorf("revoked token should not lookup; got %+v", got)
	}
}

func TestRegisterClient_Validation(t *testing.T) {
	oauthSvc, _, _ := fixture(t)
	ctx := context.Background()

	cases := []struct {
		name string
		uris []string
		ok   bool
	}{
		{"https", []string{"https://app.example.com/cb"}, true},
		{"localhost", []string{"http://localhost:9999/cb"}, true},
		{"127.0.0.1", []string{"http://127.0.0.1:9999/cb"}, true},
		{"bare http", []string{"http://evil.example/cb"}, false},
		{"missing", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := oauthSvc.RegisterClient(ctx, oauth.RegisterParams{Name: "x", RedirectURIs: c.uris})
			if (err == nil) != c.ok {
				t.Errorf("ok=%v err=%v", c.ok, err)
			}
		})
	}
}
