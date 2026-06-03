// Package extension holds the core's in-process extension seams: the small,
// stable set of interfaces (plus no-op defaults) a wrapper can implement to add
// behavior without modifying the core. It is the ONLY package outside internal/
// — everything else stays unimportable on purpose — because a separate module
// must be able to name these types and inject implementations (see
// docs/extending.md). Keep this package tiny: interfaces and no-op structs, no
// behavior. Adding to it is API surface and a potential breaking change for
// wrappers.
package extension

import (
	"context"
	"time"
)

// LimitKey identifies what is being rate-limited. Fields are populated at the
// point in the chain where identity is known; a limiter uses whichever it needs.
// ProjectID is set once the public ingest token or OAuth grant has resolved.
type LimitKey struct {
	Surface   string // "ingest" | "query" | "mcp"
	ProjectID string // resolved tenant; "" before resolution
	UserID    string // bearer surfaces only; "" for ingest
	TokenID   string // opaque credential id, for per-credential limits
	RemoteIP  string
}

// RateLimiter decides whether a request may proceed. The core ships AllowAll.
// Allow MUST be safe for concurrent use and MUST NOT block on the hot path
// beyond a small bounded check.
type RateLimiter interface {
	// Allow reports whether the request may proceed now. retryAfter is a hint
	// for the 429 Retry-After header when ok is false (zero = omit).
	Allow(ctx context.Context, key LimitKey) (ok bool, retryAfter time.Duration)
}

// AllowAll is the no-op default: every request proceeds.
type AllowAll struct{}

func (AllowAll) Allow(context.Context, LimitKey) (bool, time.Duration) { return true, 0 }

// UsageSink receives a usage signal each time the ingest pipeline durably
// accepts events for a project. The core ships Discard. RecordIngested is
// called off the request hot path (after the batch lands in ClickHouse), so a
// hosted implementation may do real work, but MUST NOT panic and SHOULD NOT
// block the flusher for long.
type UsageSink interface {
	RecordIngested(ctx context.Context, projectID string, events int)
}

// Discard is the no-op default: usage signals are dropped.
type Discard struct{}

func (Discard) RecordIngested(context.Context, string, int) {}

// Entitlement gates the read/analysis surfaces — the REST query + schema
// endpoints, the MCP query + schema tools, and the web query playground — after
// the project has been resolved. The core ships Unlimited (every project may
// analyze). It is consulted ONLY on those analysis surfaces; ingest is never
// gated, so a hosted build can keep accepting a tenant's events while denying
// analysis until they pay. AllowAnalysis MUST be safe for concurrent use and
// MUST NOT block beyond a small, bounded check.
type Entitlement interface {
	// AllowAnalysis reports whether projectID may use the analysis surfaces
	// right now. reason is a short human-readable hint surfaced on denial
	// (the 402 body / MCP tool error); empty selects a generic default. On
	// deny the core returns 402 on the API/MCP surfaces and redirects the web
	// playground to the wrapper's upgrade URL, and the handler never runs.
	AllowAnalysis(ctx context.Context, projectID string) (ok bool, reason string)
}

// Unlimited is the no-op default: every project may analyze.
type Unlimited struct{}

func (Unlimited) AllowAnalysis(context.Context, string) (bool, string) { return true, "" }
