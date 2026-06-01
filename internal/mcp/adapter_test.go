package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestRecoverToolHandler_Panic is the first line of the double-recovery
// defense: a panicking tool handler must not unwind into the transport. It
// becomes a JSON-RPC internal_error (non-nil handler error) and leaves a
// logged stack trace behind.
func TestRecoverToolHandler_Panic(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	wrapped := recoverToolHandler("boom", logger, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		panic("kaboom")
	})

	result, err := wrapped(context.Background(), mcpgo.CallToolRequest{})
	if result != nil {
		t.Fatalf("result = %v, want nil on panic", result)
	}
	if !errors.Is(err, errInternal) {
		t.Fatalf("err = %v, want errInternal", err)
	}
	log := buf.String()
	if !strings.Contains(log, "mcp tool panic") {
		t.Errorf("log missing panic line:\n%s", log)
	}
	if !strings.Contains(log, "boom") {
		t.Errorf("log missing tool name:\n%s", log)
	}
	if !strings.Contains(log, "kaboom") {
		t.Errorf("log missing panic value:\n%s", log)
	}
}

// TestRecoverToolHandler_PassThrough confirms the wrapper is transparent on the
// happy path — the inner handler's result and error flow through untouched.
func TestRecoverToolHandler_PassThrough(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	want := mcpgo.NewToolResultText("ok")
	wrapped := recoverToolHandler("fine", logger, func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return want, nil
	})
	got, err := wrapped(context.Background(), mcpgo.CallToolRequest{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != want {
		t.Fatalf("result = %v, want pass-through %v", got, want)
	}
}

// TestPanicBecomesJSONRPCError drives a panicking tool through the real
// Streamable HTTP transport: the panic must surface as a JSON-RPC
// internal_error (-32603), and the server must survive to answer the next
// request — a library bug cannot take the process down.
func TestPanicBecomesJSONRPCError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	mcpServer := server.NewMCPServer("test", "1", server.WithToolCapabilities(false))
	registerTool(mcpServer, logger, mcpgo.NewTool("boom", mcpgo.WithDescription("always panics")),
		func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			panic("kaboom")
		})
	ts := httptest.NewServer(server.NewStreamableHTTPServer(mcpServer, server.WithStateLess(true)))
	defer ts.Close()

	callBoom := func(t *testing.T) int {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom","arguments":{}}}`
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var env struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("response not JSON: %v\nbody=%s", err, raw)
		}
		if env.Error == nil {
			t.Fatalf("want a JSON-RPC error, got: %s", raw)
		}
		return env.Error.Code
	}

	if code := callBoom(t); code != mcpgo.INTERNAL_ERROR {
		t.Errorf("error code = %d, want INTERNAL_ERROR (%d)", code, mcpgo.INTERNAL_ERROR)
	}
	// Process survived: a second call still gets a well-formed error back.
	if code := callBoom(t); code != mcpgo.INTERNAL_ERROR {
		t.Errorf("second call error code = %d, want INTERNAL_ERROR (%d)", code, mcpgo.INTERNAL_ERROR)
	}
	if !strings.Contains(buf.String(), "mcp tool panic") {
		t.Errorf("expected panic log line, got:\n%s", buf.String())
	}
}
