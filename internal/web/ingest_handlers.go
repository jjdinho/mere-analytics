package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/ingest"
)

// projectKey ties the resolved project ID onto the request context after
// requirePublicToken. Downstream handlers pull it back out via projectFromCtx.
type projectCtxKey struct{}

func projectFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(projectCtxKey{}).(string)
	return v
}

// ingestBody is the request shape POST /api/v1/ingest/events accepts. Token
// carries the mere_pub_ credential in-body (the browser SDK can't set an
// Authorization header on navigator.sendBeacon at page-unload); requirePublicToken
// reads it ahead of the handler, and listing it here keeps DecodeJSON's strict
// DisallowUnknownFields from rejecting it.
type ingestBody struct {
	Token  string         `json:"token"`
	Events []ingest.Event `json:"events"`
}

// ingestResponse is what a successful (or empty-batch) request returns.
type ingestResponse struct {
	Accepted int                      `json:"accepted"`
	Rejected int                      `json:"rejected"`
	Errors   []ingest.ValidationError `json:"errors,omitempty"`
}

// requirePublicToken enforces the in-body mere_pub_… handshake for the ingest
// path. The credential rides in the JSON body's "token" field (not an
// Authorization header) so the browser SDK can authenticate over
// navigator.sendBeacon at page-unload, where headers can't be set. It must stay
// a middleware ahead of rateLimit so LimitKey.ProjectID is populated before the
// per-project bucket is consulted — for a browser SDK every end user is a
// different IP, so per-project is the only meaningful rate-limit key.
//
// It buffers the (already MaxBody+Decompress-capped) body, probes it for the
// token, resolves the project, then rebuffers the bytes so postIngest can decode
// the full envelope again. Four response-distinguishing failure modes:
//
//   - oversize body (MaxBytesError from the read) → 413.
//   - unreadable body / non-JSON → 413 / 400.
//   - missing token or non-mere_pub_ prefix → 401.
//   - PG lookup error (infrastructure failure) → 500. Never 401; conflating
//     would make a PG outage look like a credential-stuffing attack.
//   - successful prefix + hash but no row / soft-deleted project → 401.
//
// On success the resolved project ID is stashed on ctx for postIngest.
func requirePublicToken(svc *ingest.Service, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body) // body already capped by MaxBody + Decompress
			if err != nil {
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			var probe struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			token := probe.Token
			if token == "" || !strings.HasPrefix(token, auth.PublicTokenPrefix) {
				writeBearerUnauthorized(w, "invalid_request")
				return
			}
			projectID, err := svc.LookupIngestToken(r.Context(), token)
			if err != nil {
				logger.Error("ingest token lookup", "err", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if projectID == "" {
				writeBearerUnauthorized(w, "invalid_token")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw)) // rebuffer for postIngest
			ctx := context.WithValue(r.Context(), projectCtxKey{}, projectID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// postIngest is the POST /api/v1/ingest/events handler. State checks first (kill switch
// + fatal flag), then body decode + validation, then Submit + response.
//
//	disabled / fatal → 503 + Retry-After
//	body too big     → 413 (via DecodeJSON)
//	bad JSON         → 400 (via DecodeJSON)
//	empty after val  → 200 with accepted=0
//	chan full        → 503 + Retry-After: 1
//	ok               → 202 with accepted + rejected counts
func postIngest(svc *ingest.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flags := svc.Flags()
		if flags.IsDisabled() {
			w.Header().Set("Retry-After", "300")
			http.Error(w, "ingest disabled", http.StatusServiceUnavailable)
			return
		}
		if flags.IsFatal() {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "ingest down", http.StatusServiceUnavailable)
			return
		}
		body, ok := DecodeJSON[ingestBody](w, r)
		if !ok {
			return
		}
		valid, rejected := ingest.ValidateBatch(body.Events)
		if len(valid) == 0 {
			writeIngestJSON(w, http.StatusOK, ingestResponse{
				Accepted: 0,
				Rejected: len(rejected),
				Errors:   rejected,
			})
			return
		}
		projectID := projectFromCtx(r.Context())
		if err := svc.Submit(r.Context(), projectID, valid); err != nil {
			if errors.Is(err, ingest.ErrChannelFull) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "ingest channel full", http.StatusServiceUnavailable)
				return
			}
			logger.Error("ingest submit", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		writeIngestJSON(w, http.StatusAccepted, ingestResponse{
			Accepted: len(valid),
			Rejected: len(rejected),
			Errors:   rejected,
		})
	}
}

func writeIngestJSON(w http.ResponseWriter, status int, body ingestResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
