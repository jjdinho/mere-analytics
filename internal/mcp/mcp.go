// Package mcp exposes the analytics read surface (query + schema) as Model
// Context Protocol tools behind a single /mcp endpoint.
//
// The tools are thin adapters: every tenant-sensitive decision — the project
// filter, the per-request ClickHouse limits, the readonly pool, the table
// allowlist — lives in internal/query, the same package the /api/v1/* HTTP
// handlers call. MCP and HTTP are two front doors over one executor so the
// isolation contract has a single source of truth and cannot drift.
package mcp

import (
	"log/slog"
	"net/http"

	"github.com/mark3labs/mcp-go/server"

	"github.com/jjdinho/mere-analytics/extension"
	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/query"
)

const (
	serverName    = "mere-analytics"
	serverVersion = "1"
)

// Deps bundles what the MCP tools need. AuthService re-checks that the bearer
// grant's project is still visible to the granting user (soft-delete /
// membership parity with the HTTP API, decision #11); Executor and Schema are
// the shared internal/query services.
//
// Entitlement is the analysis gate (see docs/extending.md): consulted in
// authorizeProject after visibility, so an over-quota project's query + schema
// tools deny with an upgrade message. Nil defaults to the no-op
// extension.Unlimited, matching the open-source build's always-allow behavior.
type Deps struct {
	AuthService *auth.Service
	Executor    *query.Executor
	Schema      *query.SchemaProvider
	Entitlement extension.Entitlement
	Logger      *slog.Logger
}

// NewServer builds an MCP server with the query + schema tools registered.
// Each tool handler is wrapped by registerTool so a panic becomes a JSON-RPC
// internal_error instead of unwinding into the transport.
func NewServer(deps Deps) *server.MCPServer {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Entitlement == nil {
		deps.Entitlement = extension.Unlimited{}
	}
	s := server.NewMCPServer(serverName, serverVersion,
		// Static tool set — no list-changed notifications to advertise.
		server.WithToolCapabilities(false),
	)
	registerTool(s, deps.Logger, queryTool(), queryHandler(deps))
	registerTool(s, deps.Logger, schemaTool(), schemaHandler(deps))
	return s
}

// NewHTTPHandler builds the Streamable HTTP transport for mounting at /mcp.
//
// Stateless: each request is self-contained, so there is no session store to
// keep — the OAuth bearer token alone carries identity, and the caller wraps
// this with the same RequireBearer + CORS middleware as /api/v1/*. The
// request context (with the bearer's oauth.AccessContext) flows through to the
// tool handlers, which is how a tool learns which project it is scoped to.
func NewHTTPHandler(deps Deps) http.Handler {
	return server.NewStreamableHTTPServer(NewServer(deps),
		server.WithStateLess(true),
	)
}
