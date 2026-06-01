// Package e2e_test exercises the full boot path of cmd/server end-to-end:
// real PG + CH containers, the actual mere-server binary running as a
// subprocess, and HTTP probes against /healthz and /.
package e2e_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jjdinho/mere-analytics/internal/testhelpers"
)

func TestBoot_endToEnd(t *testing.T) {
	pgPool, pgCfg := testhelpers.StartPostgres(t)
	pgPool.Close()
	_, chCfg := testhelpers.StartClickHouse(t)

	binPath := buildServer(t)

	port := freeTCPPort(t)
	env := append(os.Environ(),
		"PORT="+strconv.Itoa(port),
		"POSTGRES_HOST="+pgCfg.PostgresHost,
		"POSTGRES_PORT="+strconv.Itoa(pgCfg.PostgresPort),
		"POSTGRES_DB="+pgCfg.PostgresDB,
		"POSTGRES_USER="+pgCfg.PostgresUser,
		"POSTGRES_PASSWORD="+pgCfg.PostgresPassword,
		"CLICKHOUSE_HOST="+chCfg.ClickHouseHost,
		"CLICKHOUSE_PORT="+strconv.Itoa(chCfg.ClickHousePort),
		"CLICKHOUSE_DATABASE="+chCfg.ClickHouseDatabase,
		"CLICKHOUSE_ADMIN_USER="+chCfg.ClickHouseAdminUser,
		"CLICKHOUSE_ADMIN_PASSWORD="+chCfg.ClickHouseAdminPassword,
		"CLICKHOUSE_READONLY_USER="+chCfg.ClickHouseReadonlyUser,
		"CLICKHOUSE_READONLY_PASSWORD="+chCfg.ClickHouseReadonlyPassword,
		"OAUTH_ISSUER_URL=http://127.0.0.1:"+strconv.Itoa(port),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Env = env
	// Pipe child logs through to test output so failures are diagnosable.
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	// Put the child in its own process group so SIGTERM doesn't escape.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		// Best-effort kill if the test left the process running. ProcessState
		// is set only after Wait returns; if it's nil here, the subprocess
		// is still alive and needs a SIGKILL.
		if cmd.ProcessState == nil && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
	})

	// Poll /healthz up to 30 seconds (testcontainers + migrations need time on a cold machine).
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	waitFor200(t, healthURL, 30*time.Second)

	// Healthz body.
	body := mustGetBody(t, healthURL)
	if strings.TrimSpace(body) != "ok" {
		t.Errorf("/healthz body: got %q want %q", body, "ok")
	}

	// Index page.
	body = mustGetBody(t, fmt.Sprintf("http://127.0.0.1:%d/", port))
	if !strings.Contains(body, "mere — running") {
		t.Errorf("/ body missing brand text. got:\n%s", body)
	}

	// Graceful shutdown.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-done:
		// nil exit code or signal-killed is fine; binary returned cleanly.
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == -1 {
				// Killed by signal — that's fine for SIGTERM-handled shutdown.
				return
			}
			// Allow exit code 0 or signal-shutdown; flag anything else.
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() != 0 {
				t.Errorf("server exited non-zero after SIGTERM: %v", err)
			}
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not exit within 15s of SIGTERM")
	}
}

// buildServer compiles cmd/server into a temp binary and returns its path.
// Building once is much faster than `go run` per test.
func buildServer(t *testing.T) string {
	t.Helper()
	repoRoot := findRepoRoot(t)
	out := filepath.Join(t.TempDir(), "mere-server-e2e")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	build := exec.Command("go", "build", "-o", out, "./cmd/server")
	build.Dir = repoRoot
	build.Stdout = testWriter{t}
	build.Stderr = testWriter{t}
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}
	return out
}

// findRepoRoot walks up from the test file's directory until it finds go.mod.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod walking up from cwd")
		}
		dir = parent
	}
}

// freeTCPPort asks the kernel for a free TCP port. There's a small race
// between the close and the subprocess binding, but it's the standard idiom.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func waitFor200(t *testing.T, url string, deadline time.Duration) {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for 200 from %s after %s", url, deadline)
}

func mustGetBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

type testWriter struct{ t *testing.T }

func (tw testWriter) Write(p []byte) (int, error) {
	tw.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
