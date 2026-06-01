package oauth

import "errors"

// Typed errors mapped 1:1 to RFC 6749 §5.2 error codes by the HTTP layer.
// Keep this list aligned with the strings in oauth_handlers.go.
var (
	ErrInvalidRequest       = errors.New("invalid_request")
	ErrInvalidGrant         = errors.New("invalid_grant")
	ErrUnauthorizedClient   = errors.New("unauthorized_client")
	ErrUnsupportedGrantType = errors.New("unsupported_grant_type")
	ErrAccessDenied         = errors.New("access_denied")
	ErrInvalidScope         = errors.New("invalid_scope")
	ErrServerError          = errors.New("server_error")
)
