package mcp

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// errInternal is returned to mcp-go when a tool handler panics. mcp-go maps a
// non-nil handler error to a JSON-RPC error with code INTERNAL_ERROR (-32603),
// so the caller sees a well-formed protocol error rather than a dropped
// connection. The message is deliberately opaque — the detail is in the log.
var errInternal = errors.New("internal error")

// registerTool adds a tool whose handler is wrapped so a panic can never
// escape into the JSON-RPC transport. This is the first of two recovery
// layers: a panic here is logged with its stack and converted to a JSON-RPC
// internal_error; recoverMiddleware around the whole mux is the second line of
// defense, catching a panic in mcp-go's own routing code so the process
// survives a library bug.
func registerTool(s *server.MCPServer, logger *slog.Logger, tool mcpgo.Tool, handler server.ToolHandlerFunc) {
	s.AddTool(tool, recoverToolHandler(tool.Name, logger, handler))
}

func recoverToolHandler(name string, logger *slog.Logger, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (result *mcpgo.CallToolResult, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("mcp tool panic",
					"tool", name,
					"err", r,
					"stack", string(debug.Stack()),
				)
				result = nil
				err = errInternal
			}
		}()
		return next(ctx, req)
	}
}
