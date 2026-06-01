package web

import (
	"context"
	"encoding/json"
	"errors"
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

// ingestBody is the request shape POST /v1/ingest accepts.
type ingestBody struct {
	Events []ingest.Event `json:"events"`
}

// ingestResponse is what a successful (or empty-batch) request returns.
type ingestResponse struct {
	Accepted int                       `json:"accepted"`
	Rejected int                       `json:"rejected"`
	Errors   []ingest.ValidationError  `json:"errors,omitempty"`
}

// requirePublicToken enforces the Authorization: Bearer mere_pub_… handshake
// for the ingest path. Three response-distinguishing failure modes:
//
//   - missing Authorization or non-Bearer / non-mere_pub_ prefix → 401.
//   - PG lookup error (infrastructure failure) → 500. Never 401; conflating
//     would make a PG outage look like a credential-stuffing attack.
//   - successful prefix + hash but no row / soft-deleted project → 401.
//
// On success the resolved project ID is stashed on ctx for postIngest.
func requirePublicToken(svc *ingest.Service, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearer(r.Header.Get("Authorization"))
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
			ctx := context.WithValue(r.Context(), projectCtxKey{}, projectID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// postIngest is the POST /v1/ingest handler. State checks first (kill switch
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
