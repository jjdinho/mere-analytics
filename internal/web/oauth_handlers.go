package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/views"
)

// OAuth handlers implement a PKCE-only authorization-code flow:
//
//   /oauth/register     RFC 7591  → 201 {client_id, redirect_uris}
//   /oauth/authorize    RFC 6749  → consent page or 302 redirect with code
//   /oauth/token        RFC 6749  → 200 {access_token, token_type, expires_in, scope}
//   /.well-known/oauth-authorization-server   RFC 8414  → discovery JSON
//
// Error handling follows RFC 6749 §5.2: invalid_request, invalid_grant, etc.
// Errors at the authorize endpoint redirect to the registered redirect_uri
// with ?error=…&state=… EXCEPT when the client_id or redirect_uri are
// themselves invalid — in that case we can't trust the redirect target and
// must respond directly with HTTP 400 (RFC 6749 §4.1.2.1).

// getOAuthMetadata serves the RFC 8414 discovery document. The JSON is
// computed once per process from cfg and reused on every request.
func getOAuthMetadata(issuer string) http.HandlerFunc {
	doc, _ := json.Marshal(map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/oauth/authorize",
		"token_endpoint":                        issuer + "/oauth/token",
		"registration_endpoint":                 issuer + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{oauth.ScopeAPI},
	})
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}
}

// registerRequest is the RFC 7591 client-registration body. Only
// redirect_uris is required.
type registerRequest struct {
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// postOAuthRegister handles RFC 7591 dynamic client registration.
func postOAuthRegister(svc *oauth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeOAuthJSONError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON body")
			return
		}
		client, err := svc.RegisterClient(r.Context(), oauth.RegisterParams{
			Name:         req.ClientName,
			RedirectURIs: req.RedirectURIs,
		})
		if errors.Is(err, oauth.ErrInvalidRequest) {
			writeOAuthJSONError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
			return
		}
		if err != nil {
			logger.Error("oauth register", "err", err)
			writeOAuthJSONError(w, http.StatusInternalServerError, "server_error", "internal error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":                client.ID,
			"client_name":              client.Name,
			"redirect_uris":            client.RedirectURIs,
			"grant_types":              []string{"authorization_code"},
			"response_types":           []string{"code"},
			"token_endpoint_auth_method": "none",
		})
	}
}

// authorizeParams captures the parsed authorize-endpoint query (RFC 6749
// §4.1.1 + RFC 7636).
type authorizeParams struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

func parseAuthorizeParams(values url.Values) authorizeParams {
	return authorizeParams{
		ResponseType:        values.Get("response_type"),
		ClientID:            values.Get("client_id"),
		RedirectURI:         values.Get("redirect_uri"),
		Scope:               values.Get("scope"),
		State:               values.Get("state"),
		CodeChallenge:       values.Get("code_challenge"),
		CodeChallengeMethod: values.Get("code_challenge_method"),
	}
}

// getOAuthAuthorize renders the consent page. Unauthenticated users are
// bounced to /login?next=<authorize URL>; once they come back authenticated
// the same URL renders the consent picker.
func getOAuthAuthorize(authSvc *auth.Service, oauthSvc *oauth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params := parseAuthorizeParams(r.URL.Query())
		client, ok := validateAuthorizeRequest(w, r, oauthSvc, params, logger)
		if !ok {
			return
		}

		sess := auth.SessionFrom(r.Context())
		if sess == nil {
			// Bounce through /login, preserving the original authorize URL so
			// the user lands back here after authentication.
			next := r.URL.RequestURI()
			http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}

		viewer := auth.ViewerFrom(r.Context())
		teams, err := viewer.Teams(r.Context()).List()
		if err != nil {
			logger.Error("oauth authorize: list teams", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		teamIDs := make([]string, len(teams))
		for i, t := range teams {
			teamIDs[i] = t.ID
		}
		projects, err := viewer.Projects(r.Context()).ListForTeams(teamIDs)
		if err != nil {
			logger.Error("oauth authorize: list projects", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		renderOAuthConsent(w, r, sess, views.OAuthConsentData{
			ClientID:            client.ID,
			ClientName:          client.Name,
			RedirectURI:         params.RedirectURI,
			State:               params.State,
			Scope:               effectiveScope(params.Scope),
			CodeChallenge:       params.CodeChallenge,
			CodeChallengeMethod: params.CodeChallengeMethod,
			Projects:            projects,
		})
	}
}

// postOAuthAuthorize handles the user's approve/deny decision. By this point
// the user is authenticated (the route is behind requireSession). We re-
// validate the authorize params from form values because the consent page
// echoes them via hidden fields — a tampered POST is treated like a fresh
// authorize call and validated the same way.
func postOAuthAuthorize(authSvc *auth.Service, oauthSvc *oauth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		params := parseAuthorizeParams(r.PostForm)
		client, ok := validateAuthorizeRequest(w, r, oauthSvc, params, logger)
		if !ok {
			return
		}

		decision := r.PostForm.Get("decision")
		if decision == "deny" {
			redirectWithError(w, r, params.RedirectURI, "access_denied", params.State)
			return
		}
		if decision != "approve" {
			redirectWithError(w, r, params.RedirectURI, "invalid_request", params.State)
			return
		}

		projectID := r.PostForm.Get("project_id")
		if projectID == "" {
			redirectWithError(w, r, params.RedirectURI, "invalid_request", params.State)
			return
		}

		viewer := auth.ViewerFrom(r.Context())
		if _, err := viewer.Projects(r.Context()).ByID(projectID); err != nil {
			if errors.Is(err, auth.ErrNotVisible) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			logger.Error("oauth authorize: project visibility", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		sess := auth.SessionFrom(r.Context())
		code, err := oauthSvc.IssueCode(r.Context(), oauth.IssueCodeParams{
			ClientID:            client.ID,
			UserID:              sess.UserID,
			ProjectID:           projectID,
			RedirectURI:         params.RedirectURI,
			Scope:               effectiveScope(params.Scope),
			CodeChallenge:       params.CodeChallenge,
			CodeChallengeMethod: params.CodeChallengeMethod,
		})
		if err != nil {
			logger.Error("oauth issue code", "err", err)
			redirectWithError(w, r, params.RedirectURI, "server_error", params.State)
			return
		}

		q := url.Values{}
		q.Set("code", code)
		if params.State != "" {
			q.Set("state", params.State)
		}
		http.Redirect(w, r, appendQuery(params.RedirectURI, q), http.StatusSeeOther)
	}
}

// tokenRequest captures /oauth/token form values.
type tokenRequest struct {
	GrantType    string
	Code         string
	RedirectURI  string
	ClientID     string
	CodeVerifier string
}

// postOAuthToken consumes the authorization code and mints an access token.
func postOAuthToken(svc *oauth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeOAuthJSONError(w, http.StatusBadRequest, "invalid_request", "could not parse form")
			return
		}
		req := tokenRequest{
			GrantType:    r.PostForm.Get("grant_type"),
			Code:         r.PostForm.Get("code"),
			RedirectURI:  r.PostForm.Get("redirect_uri"),
			ClientID:     r.PostForm.Get("client_id"),
			CodeVerifier: r.PostForm.Get("code_verifier"),
		}
		if req.GrantType != "authorization_code" {
			writeOAuthJSONError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
			return
		}
		if req.ClientID == "" {
			writeOAuthJSONError(w, http.StatusBadRequest, "invalid_request", "client_id required")
			return
		}

		consumed, err := svc.ConsumeCode(r.Context(), oauth.ConsumeCodeParams{
			Code:         req.Code,
			ClientID:     req.ClientID,
			RedirectURI:  req.RedirectURI,
			CodeVerifier: req.CodeVerifier,
		})
		if errors.Is(err, oauth.ErrInvalidGrant) {
			writeOAuthJSONError(w, http.StatusBadRequest, "invalid_grant", "code is invalid or already used")
			return
		}
		if errors.Is(err, oauth.ErrInvalidRequest) {
			writeOAuthJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err != nil {
			logger.Error("oauth consume code", "err", err)
			writeOAuthJSONError(w, http.StatusInternalServerError, "server_error", "internal error")
			return
		}

		plaintext, expiresAt, err := svc.IssueAccessToken(r.Context(), oauth.IssueAccessTokenParams{
			ClientID:  consumed.ClientID,
			UserID:    consumed.UserID,
			ProjectID: consumed.ProjectID,
			Scope:     consumed.Scope,
		})
		if err != nil {
			logger.Error("oauth issue access token", "err", err)
			writeOAuthJSONError(w, http.StatusInternalServerError, "server_error", "internal error")
			return
		}

		ttlSeconds := int(expiresAt.Sub(svc.Now()).Seconds())
		if ttlSeconds < 1 {
			ttlSeconds = 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": plaintext,
			"token_type":   "Bearer",
			"expires_in":   ttlSeconds,
			"scope":        consumed.Scope,
		})
	}
}

// validateAuthorizeRequest enforces the spec-mandated invariants on an
// authorize request. Returns (client, true) when the request can proceed.
// On failure it writes the appropriate response itself and returns
// (Client{}, false).
//
// The contract here matches RFC 6749 §4.1.2.1: if the client_id or
// redirect_uri are themselves invalid, respond with HTTP 400 (we cannot
// trust the redirect target). All other errors redirect to the registered
// URI with ?error=…&state=….
func validateAuthorizeRequest(w http.ResponseWriter, r *http.Request, svc *oauth.Service, params authorizeParams, logger *slog.Logger) (oauth.Client, bool) {
	if params.ClientID == "" {
		http.Error(w, "client_id required", http.StatusBadRequest)
		return oauth.Client{}, false
	}
	client, err := svc.LookupClient(r.Context(), params.ClientID)
	if errors.Is(err, oauth.ErrUnauthorizedClient) {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return oauth.Client{}, false
	}
	if err != nil {
		logger.Error("oauth authorize: lookup client", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return oauth.Client{}, false
	}
	if params.RedirectURI == "" || !client.RedirectURIAllowed(params.RedirectURI) {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return oauth.Client{}, false
	}

	if params.ResponseType != "code" {
		redirectWithError(w, r, params.RedirectURI, "unsupported_response_type", params.State)
		return oauth.Client{}, false
	}
	if params.CodeChallenge == "" {
		redirectWithError(w, r, params.RedirectURI, "invalid_request", params.State)
		return oauth.Client{}, false
	}
	if params.CodeChallengeMethod != oauth.CodeChallengeMethodS256 {
		redirectWithError(w, r, params.RedirectURI, "invalid_request", params.State)
		return oauth.Client{}, false
	}
	scope := effectiveScope(params.Scope)
	if scope != oauth.ScopeAPI {
		redirectWithError(w, r, params.RedirectURI, "invalid_scope", params.State)
		return oauth.Client{}, false
	}
	return client, true
}

func effectiveScope(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return oauth.ScopeAPI
	}
	return s
}

func redirectWithError(w http.ResponseWriter, r *http.Request, redirectURI, errCode, state string) {
	q := url.Values{}
	q.Set("error", errCode)
	if state != "" {
		q.Set("state", state)
	}
	http.Redirect(w, r, appendQuery(redirectURI, q), http.StatusSeeOther)
}

// appendQuery merges q onto uri. RFC 6749 §3.1.2 allows a registered
// redirect_uri to already carry a query string, so we pick `&` vs `?` from
// the existing URI to avoid producing a malformed `...?env=prod?code=...`
// URL on the OAuth success or error redirect.
func appendQuery(uri string, q url.Values) string {
	encoded := q.Encode()
	if encoded == "" {
		return uri
	}
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	if strings.HasSuffix(uri, "?") || strings.HasSuffix(uri, "&") {
		sep = ""
	}
	return uri + sep + encoded
}

func writeOAuthJSONError(w http.ResponseWriter, status int, errCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             errCode,
		"error_description": description,
	})
}

func renderOAuthConsent(w http.ResponseWriter, r *http.Request, sess *auth.Session, data views.OAuthConsentData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.OAuthConsent(sess, data).Render(r.Context(), w)
}

