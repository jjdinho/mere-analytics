package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jjdinho/mere-analytics/internal/idgen"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
)

// Client mirrors db.OauthClient at the application boundary so handlers don't
// import the sqlc package for this single struct.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
}

// RegisterParams is the validated payload for dynamic client registration
// (RFC 7591). Name is optional (empty string → "unnamed client"); redirect
// URIs are required.
type RegisterParams struct {
	Name         string
	RedirectURIs []string
}

// RegisterClient validates the params and inserts a new public client. The
// returned Client.ID is the public client_id the caller hands to the
// requester.
//
// Redirect-URI rules (RFC 8252 §7.3 + MCP guidance):
//   - HTTPS hosts allowed
//   - http://localhost / http://127.0.0.1 allowed (loopback redirect)
//   - everything else rejected (no fragments, no bare HTTP)
func (s *Service) RegisterClient(ctx context.Context, p RegisterParams) (Client, error) {
	if len(p.RedirectURIs) == 0 {
		return Client{}, fmt.Errorf("%w: redirect_uris required", ErrInvalidRequest)
	}
	for _, u := range p.RedirectURIs {
		if err := validateRedirectURI(u); err != nil {
			return Client{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = "unnamed client"
	}

	row, err := s.queries.InsertOAuthClient(ctx, db.InsertOAuthClientParams{
		ID:           idgen.New(),
		Name:         name,
		RedirectUris: p.RedirectURIs,
	})
	if err != nil {
		return Client{}, fmt.Errorf("register client: %w", err)
	}
	return Client{ID: row.ID, Name: row.Name, RedirectURIs: row.RedirectUris}, nil
}

// LookupClient returns the client by ID. pgx.ErrNoRows becomes
// ErrUnauthorizedClient so the handler can map to the right OAuth error code
// without distinguishing "doesn't exist" from "you can't have it".
func (s *Service) LookupClient(ctx context.Context, id string) (Client, error) {
	row, err := s.queries.GetOAuthClientByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Client{}, ErrUnauthorizedClient
		}
		return Client{}, fmt.Errorf("lookup client: %w", err)
	}
	return Client{ID: row.ID, Name: row.Name, RedirectURIs: row.RedirectUris}, nil
}

// RedirectURIAllowed reports whether uri exactly matches one of the client's
// registered redirect URIs.
func (c Client) RedirectURIAllowed(uri string) bool {
	for _, registered := range c.RedirectURIs {
		if registered == uri {
			return true
		}
	}
	return false
}

func validateRedirectURI(raw string) error {
	if raw == "" {
		return fmt.Errorf("redirect_uri is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("redirect_uri parse: %w", err)
	}
	if u.Fragment != "" {
		return fmt.Errorf("redirect_uri must not contain a fragment")
	}
	switch u.Scheme {
	case "https":
		if u.Host == "" {
			return fmt.Errorf("redirect_uri https must include a host")
		}
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" {
			return nil
		}
		return fmt.Errorf("redirect_uri http only allowed for loopback (got host %q)", host)
	default:
		return fmt.Errorf("redirect_uri scheme %q not allowed", u.Scheme)
	}
}
