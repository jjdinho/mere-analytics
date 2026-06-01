package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/idgen"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
)

// accessTokenRandomBytes is the entropy per access token. 32 bytes →
// 43 base64url chars. No prefix — the bearer header carries the raw token,
// and there's no scanner-visible value in tagging it.
const accessTokenRandomBytes = 32

// AccessToken is the application-layer view of an oauth_access_tokens row.
type AccessToken struct {
	ID        string
	ClientID  string
	UserID    string
	ProjectID string
	Scope     string
	ExpiresAt time.Time
}

// IssueAccessTokenParams is the bag the token handler hands here after a
// successful code consume.
type IssueAccessTokenParams struct {
	ClientID  string
	UserID    string
	ProjectID string
	Scope     string
}

// IssueAccessToken mints a fresh access token, persists its hash, and returns
// the plaintext + expiry. The plaintext is shown to the requester once via the
// token-endpoint response and never persisted.
func (s *Service) IssueAccessToken(ctx context.Context, p IssueAccessTokenParams) (plaintext string, expiresAt time.Time, err error) {
	if p.Scope == "" {
		p.Scope = ScopeAPI
	}

	var raw [accessTokenRandomBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("issue access token: read random: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(raw[:])
	hashHex := auth.HashToken(plaintext)
	expiresAt = s.now().Add(s.AccessTokenTTL)

	if err := s.queries.InsertOAuthAccessToken(ctx, db.InsertOAuthAccessTokenParams{
		ID:        idgen.New(),
		TokenHash: hashHex,
		ClientID:  p.ClientID,
		UserID:    p.UserID,
		ProjectID: p.ProjectID,
		Scope:     p.Scope,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("issue access token: insert: %w", err)
	}
	return plaintext, expiresAt, nil
}

// LookupActiveAccessToken resolves a plaintext bearer to its bound identity.
// Returns nil + nil error when the token is unknown / expired / revoked so
// the middleware can return a uniform 401 without leaking which.
//
// Pre-DB rejection: anything that starts with PublicTokenPrefix is rejected
// without a query. Public ingest tokens live in the JS snippet — they must
// never grant /v1/* + /mcp access, and the partial-unique-hash index would
// otherwise let a collision attempt churn DB time.
func (s *Service) LookupActiveAccessToken(ctx context.Context, plaintext string) (*AccessToken, error) {
	if plaintext == "" {
		return nil, nil
	}
	if strings.HasPrefix(plaintext, auth.PublicTokenPrefix) {
		return nil, nil
	}
	hashHex := auth.HashToken(plaintext)
	row, err := s.queries.GetActiveOAuthAccessTokenByHash(ctx, hashHex)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup access token: %w", err)
	}
	return &AccessToken{
		ID:        row.ID,
		ClientID:  row.ClientID,
		UserID:    row.UserID,
		ProjectID: row.ProjectID,
		Scope:     row.Scope,
		ExpiresAt: row.ExpiresAt.Time,
	}, nil
}
