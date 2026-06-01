package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(Handler(Options{Logger: discardLogger(), Version: "v1.2.3-test"}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q want application/json", ct)
	}
	var body healthzPayload
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status field: got %q want ok", body.Status)
	}
	// The build-time version stamp surfaces in /healthz (step 8) so operators
	// can confirm what's deployed from a probe.
	if body.Version != "v1.2.3-test" {
		t.Errorf("version field: got %q want v1.2.3-test", body.Version)
	}
	if body.IngestDisabled {
		t.Error("ingest_disabled: got true want false (no ingest service wired)")
	}
}

func TestIndex(t *testing.T) {
	srv := httptest.NewServer(Handler(Options{Logger: discardLogger()}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "mere — running") {
		t.Errorf("body missing brand text. got: %s", body)
	}
}

func TestLogMiddleware_skipsHealthz(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	srv := httptest.NewServer(Handler(Options{Logger: logger}))
	t.Cleanup(srv.Close)

	// /healthz should not produce a log line.
	if _, err := http.Get(srv.URL + "/healthz"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "/healthz") {
		t.Errorf("log middleware should skip /healthz, got: %s", buf.String())
	}
	// / should produce a log line.
	buf.Reset()
	if _, err := http.Get(srv.URL + "/"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `path=/`) {
		t.Errorf("log middleware should log /, got: %s", buf.String())
	}
}

func TestRecoverMiddleware_panicReturns500(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	})
	handler := recoverMiddleware(discardLogger())(mux)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/boom")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status: got %d want 500", resp.StatusCode)
	}
}
