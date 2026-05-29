package auth_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jjdinho/mere-analytics/internal/auth"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

// resetPasswordScriptPath walks up from the test's working directory to find
// the operator script, matching findRepoRoot's idiom in e2e/boot_test.go.
func resetPasswordScriptPath(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		candidate := filepath.Join(dir, "scripts", "operator", "reset-password.sql")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find scripts/operator/reset-password.sql walking up from cwd")
		}
		dir = parent
	}
}

// runPsqlScript invokes the host's psql against the testcontainer using the
// connection params from cfg. Skips the whole test if psql isn't installed
// (CI environments without postgres-client should still run unit tests).
func runPsqlScript(t *testing.T, host, user, db, password, scriptPath string, port int, vars map[string]string) (stdout, stderr string, exitCode int) {
	t.Helper()
	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql not installed; skipping operator script integration test")
	}
	args := []string{
		"-h", host,
		"-p", itoa(port),
		"-U", user,
		"-d", db,
	}
	for k, v := range vars {
		args = append(args, "-v", k+"="+v)
	}
	args = append(args, "-f", scriptPath)

	cmd := exec.Command("psql", args...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+password)

	var sout, serr strings.Builder
	cmd.Stdout = &sout
	cmd.Stderr = &serr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return sout.String(), serr.String(), ee.ExitCode()
		}
		t.Fatalf("psql exec: %v", err)
	}
	return sout.String(), serr.String(), 0
}

func itoa(n int) string {
	// Avoid pulling in strconv here; n is always a TCP port.
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestResetPassword_unknownEmail_exitsNonZero(t *testing.T) {
	pool, cfg := testhelpers.StartPostgres(t)
	_ = pool
	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	if err := mmigrate.Run(context.Background(), "pg", drv, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	script := resetPasswordScriptPath(t)
	stdout, stderr, code := runPsqlScript(t,
		cfg.PostgresHost, cfg.PostgresUser, cfg.PostgresDB, cfg.PostgresPassword,
		script, cfg.PostgresPort,
		map[string]string{
			"email":    "nobody@example.com",
			"password": "correct horse battery staple",
		},
	)
	if code == 0 {
		t.Errorf("unknown email exited 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "no user with email") {
		t.Errorf("expected explanatory error; stderr=%q", stderr)
	}
}

func TestResetPassword_knownEmail_succeedsAndUserCanLogin(t *testing.T) {
	pool, cfg := testhelpers.StartPostgres(t)
	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	if err := mmigrate.Run(context.Background(), "pg", drv, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	svc := auth.NewService(pool)
	ctx := context.Background()
	if _, err := svc.Signup(ctx, auth.SignupRequest{
		Email: "owner@example.com", Password: "original password value",
	}); err != nil {
		t.Fatalf("Signup: %v", err)
	}

	script := resetPasswordScriptPath(t)
	stdout, stderr, code := runPsqlScript(t,
		cfg.PostgresHost, cfg.PostgresUser, cfg.PostgresDB, cfg.PostgresPassword,
		script, cfg.PostgresPort,
		map[string]string{
			"email":    "owner@example.com",
			"password": "totally new pw 999",
		},
	)
	if code != 0 {
		t.Fatalf("reset failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// Old password no longer works; new one does.
	if _, err := svc.Authenticate(ctx, "owner@example.com", "original password value"); err == nil {
		t.Errorf("old password still authenticates")
	}
	if _, err := svc.Authenticate(ctx, "owner@example.com", "totally new pw 999"); err != nil {
		t.Errorf("new password does not authenticate: %v", err)
	}
}

func TestResetPassword_caseInsensitiveEmail(t *testing.T) {
	pool, cfg := testhelpers.StartPostgres(t)
	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	if err := mmigrate.Run(context.Background(), "pg", drv, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	svc := auth.NewService(pool)
	if _, err := svc.Signup(context.Background(), auth.SignupRequest{
		Email: "Mixed@Case.Example", Password: "original password value",
	}); err != nil {
		t.Fatalf("Signup: %v", err)
	}

	script := resetPasswordScriptPath(t)
	stdout, stderr, code := runPsqlScript(t,
		cfg.PostgresHost, cfg.PostgresUser, cfg.PostgresDB, cfg.PostgresPassword,
		script, cfg.PostgresPort,
		map[string]string{
			"email":    "MIXED@case.EXAMPLE",
			"password": "totally new pw 999",
		},
	)
	if code != 0 {
		t.Fatalf("reset: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
