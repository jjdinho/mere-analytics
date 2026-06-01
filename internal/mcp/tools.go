package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/query"
)

// mcpMaxRows bounds a tool's result set. Matches the web playground cap: an
// MCP result is read into an LLM context window, so a million-row passthrough
// makes no sense. The model is told to add a LIMIT when it trips this.
const mcpMaxRows = 1000

// queryResponse is the JSON envelope the query tool returns — the same shape
// the /api/v1/projects/:id/query HTTP endpoint streams, so the two front doors
// speak one language.
type queryResponse struct {
	Columns []query.Column `json:"columns"`
	Rows    [][]any        `json:"rows"`
	Stats   query.Stats    `json:"stats"`
}

func queryTool() mcpgo.Tool {
	return mcpgo.NewTool("query",
		mcpgo.WithDescription("Run a read-only SQL query (ClickHouse dialect) against this project's analytics events. "+
			"The query is automatically scoped to the project bound to your access token — you cannot read another "+
			"project's data. Events live in the table events_raw_v1. Returns JSON "+
			`{"columns":[{"name","type"}],"rows":[[...]],"stats":{"rows","elapsed_ms"}}. `+
			"Always add a LIMIT; results are capped at 1000 rows."),
		mcpgo.WithString("sql",
			mcpgo.Description("The SQL SELECT statement to run, e.g. "+
				"SELECT event, count() FROM events_raw_v1 GROUP BY event ORDER BY count() DESC LIMIT 20"),
			mcpgo.Required(),
		),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(true),
	)
}

func queryHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		projectID, denied, err := authorizeProject(ctx, deps)
		if err != nil {
			return nil, err
		}
		if denied != nil {
			return denied, nil
		}
		sqlText, err := req.RequireString("sql")
		if err != nil {
			return mcpgo.NewToolResultError("sql is required"), nil
		}
		result, err := deps.Executor.Collect(ctx, projectID, sqlText, mcpMaxRows)
		if err != nil {
			if errors.Is(err, query.ErrRowLimitExceeded) {
				return mcpgo.NewToolResultError("Query returned more than 1000 rows. Add a LIMIT and run it again."), nil
			}
			if errors.Is(err, query.ErrEmptySQL) {
				return mcpgo.NewToolResultError("sql is required"), nil
			}
			// ClickHouse errors (parse, timeout, memory) go back verbatim —
			// power users want them, same as the HTTP API.
			return mcpgo.NewToolResultError(clickhouseErrorMessage(err)), nil
		}
		payload, err := json.Marshal(queryResponse{
			Columns: result.Columns,
			Rows:    result.Rows,
			Stats:   result.Stats,
		})
		if err != nil {
			return nil, err
		}
		return mcpgo.NewToolResultText(string(payload)), nil
	}
}

func schemaTool() mcpgo.Tool {
	return mcpgo.NewTool("schema",
		mcpgo.WithDescription("Return the queryable table and column catalog for this project's analytics data. "+
			"Use it to discover what you can SELECT before calling the query tool. Returns JSON "+
			`{"tables":[{"name","columns":[{"name","type"}]}]}.`),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithIdempotentHintAnnotation(true),
	)
}

func schemaHandler(deps Deps) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		_, denied, err := authorizeProject(ctx, deps)
		if err != nil {
			return nil, err
		}
		if denied != nil {
			return denied, nil
		}
		catalog, err := deps.Schema.Schema(ctx)
		if err != nil {
			return mcpgo.NewToolResultError(clickhouseErrorMessage(err)), nil
		}
		payload, err := json.Marshal(catalog)
		if err != nil {
			return nil, err
		}
		return mcpgo.NewToolResultText(string(payload)), nil
	}
}

// authorizeProject resolves the project from the bearer grant and verifies it
// is still visible to the granting user — soft-deleted or membership-revoked
// projects deny here, mirroring the 404 the HTTP API returns (decision #11).
//
// On success it returns the project ID. A non-nil *CallToolResult is a
// tool-error to hand straight back to the model (unauthorized / not found). A
// non-nil error is an infrastructure failure (e.g. Postgres down) that should
// surface as a JSON-RPC internal_error, not read like an empty result.
func authorizeProject(ctx context.Context, deps Deps) (string, *mcpgo.CallToolResult, error) {
	ac := oauth.FromContext(ctx)
	if ac == nil || ac.ProjectID == "" {
		return "", mcpgo.NewToolResultError("unauthorized"), nil
	}
	v := auth.NewViewer(deps.AuthService, ac.UserID)
	if _, err := v.Projects(ctx).ByID(ac.ProjectID); err != nil {
		if errors.Is(err, auth.ErrNotVisible) {
			return "", mcpgo.NewToolResultError("project not found"), nil
		}
		return "", nil, err
	}
	return ac.ProjectID, nil, nil
}

// clickhouseErrorMessage strips the executor's internal wrapping prefix so the
// model sees the raw ClickHouse message, matching the HTTP handler.
func clickhouseErrorMessage(err error) string {
	return strings.TrimPrefix(err.Error(), "clickhouse query: ")
}
