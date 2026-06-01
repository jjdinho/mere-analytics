package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/idgen"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
)

// codeRandomBytes is the entropy per authorization code before base64url
// encoding. 32 bytes → 43 chars; same shape as session/api/invite tokens.
const codeRandomBytes = 32

// IssueCodeParams is the bag the consent handler hands to IssueCode after the
// user approves on /oauth/authorize.
type IssueCodeParams struct {
	ClientID            string
	UserID              string
	ProjectID           string
	RedirectURI         string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// ConsumeCodeParams is what /oauth/token submits.
type ConsumeCodeParams struct {
	Code         string
	ClientID     string
	RedirectURI  string
	CodeVerifier string
}

// IssueCode mints a fresh authorization code bound to (client, user, project,
// redirect_uri, scope, PKCE challenge). Returns the plaintext code (the
// caller embeds it in the redirect URI). The hash is stored; the plaintext is
// not.
func (s *Service) IssueCode(ctx context.Context, p IssueCodeParams) (plaintext string, err error) {
	if p.CodeChallengeMethod != CodeChallengeMethodS256 {
		return "", fmt.Errorf("%w: code_challenge_method must be %s", ErrInvalidRequest, CodeChallengeMethodS256)
	}
	if p.CodeChallenge == "" {
		return "", fmt.Errorf("%w: code_challenge required", ErrInvalidRequest)
	}
	if p.Scope == "" {
		p.Scope = ScopeAPI
	}

	var raw [codeRandomBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("issue code: read random: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(raw[:])
	hashHex := auth.HashToken(plaintext)
	expires := s.now().Add(s.AuthorizationCodeTTL)

	if err := s.queries.InsertOAuthCode(ctx, db.InsertOAuthCodeParams{
		ID:                  idgen.New(),
		CodeHash:            hashHex,
		ClientID:            p.ClientID,
		UserID:              p.UserID,
		ProjectID:           p.ProjectID,
		RedirectUri:         p.RedirectURI,
		Scope:               p.Scope,
		CodeChallenge:       p.CodeChallenge,
		CodeChallengeMethod: p.CodeChallengeMethod,
		ExpiresAt:           pgtype.Timestamptz{Time: expires, Valid: true},
	}); err != nil {
		return "", fmt.Errorf("issue code: insert: %w", err)
	}
	return plaintext, nil
}

// ConsumedCode is what ConsumeCode returns to the token handler so it can
// issue the access token bound to the same (user, project, scope).
type ConsumedCode struct {
	ClientID  string
	UserID    string
	ProjectID string
	Scope     string
}

// ConsumeCode validates the bound (client_id, redirect_uri, PKCE) tuple and
// only then marks the code used. The two-step lookup-then-update lets a
// failed PKCE / client / redirect check leave the code intact so a legitimate
// retry (typo'd verifier, retried CLI request) doesn't force the user back
// through /oauth/authorize.
//
// One-shot is enforced by the UPDATE's `WHERE used_at IS NULL` predicate:
// a parallel /oauth/token call racing on the same code sees RowsAffected
// == 0 on the loser, which maps to invalid_grant. No transaction is needed
// — the UPDATE itself is the atomic boundary.
func (s *Service) ConsumeCode(ctx context.Context, p ConsumeCodeParams) (ConsumedCode, error) {
	if p.Code == "" || p.CodeVerifier == "" {
		return ConsumedCode{}, fmt.Errorf("%w: code and code_verifier required", ErrInvalidRequest)
	}
	hashHex := auth.HashToken(p.Code)

	row, err := s.queries.GetActiveOAuthCodeByHash(ctx, hashHex)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConsumedCode{}, ErrInvalidGrant
	}
	if err != nil {
		return ConsumedCode{}, fmt.Errorf("consume code: lookup: %w", err)
	}

	if row.ClientID != p.ClientID {
		return ConsumedCode{}, ErrInvalidGrant
	}
	if row.RedirectUri != p.RedirectURI {
		return ConsumedCode{}, ErrInvalidGrant
	}
	if !VerifyS256(p.CodeVerifier, row.CodeChallenge) {
		return ConsumedCode{}, ErrInvalidGrant
	}

	affected, err := s.queries.MarkOAuthCodeUsed(ctx, row.ID)
	if err != nil {
		return ConsumedCode{}, fmt.Errorf("consume code: mark used: %w", err)
	}
	if affected == 0 {
		// Concurrent /oauth/token won the race — fall through to the same
		// invalid_grant the lookup-miss path produces.
		return ConsumedCode{}, ErrInvalidGrant
	}
	return ConsumedCode{
		ClientID:  row.ClientID,
		UserID:    row.UserID,
		ProjectID: row.ProjectID,
		Scope:     row.Scope,
	}, nil
}
