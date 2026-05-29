package auth_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jjdinho/mere-analytics/internal/auth"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

func createUserScriptPath(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		candidate := filepath.Join(dir, "scripts", "operator", "create-user.sql")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find scripts/operator/create-user.sql walking up from cwd")
		}
		dir = parent
	}
}

func TestCreateUser_happyPath_userTeamMembership(t *testing.T) {
	pool, cfg := testhelpers.StartPostgres(t)
	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	if err := mmigrate.Run(context.Background(), "pg", drv, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	script := createUserScriptPath(t)
	stdout, stderr, code := runPsqlScript(t,
		cfg.PostgresHost, cfg.PostgresUser, cfg.PostgresDB, cfg.PostgresPassword,
		script, cfg.PostgresPort,
		map[string]string{
			"email":    "owner@example.com",
			"password": "initial password 12",
		},
	)
	if code != 0 {
		t.Fatalf("create-user failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	svc := auth.NewService(pool)
	ctx := context.Background()

	user, err := svc.Authenticate(ctx, "owner@example.com", "initial password 12")
	if err != nil {
		t.Fatalf("authenticate after create-user: %v", err)
	}
	if !user.MustChangePassword {
		t.Errorf("must_change_password = false; expected true after operator-created user")
	}

	// Personal team + membership landed in the same tx as the user.
	q := svc.Queries()
	dbUser, err := q.GetUserByEmail(ctx, "owner@example.com")
	if err != nil {
		t.Fatalf("get user by email: %v", err)
	}
	teams, err := q.ListTeamsForUser(ctx, dbUser.ID)
	if err != nil {
		t.Fatalf("list teams for user: %v", err)
	}
	if len(teams) != 1 {
		t.Fatalf("expected 1 team for new user, got %d", len(teams))
	}
	if want := "owner's team"; teams[0].Name != want {
		t.Errorf("team name = %q, want %q", teams[0].Name, want)
	}
}

func TestCreateUser_duplicateEmail_exitsNonZero(t *testing.T) {
	pool, cfg := testhelpers.StartPostgres(t)
	_ = pool
	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	if err := mmigrate.Run(context.Background(), "pg", drv, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	script := createUserScriptPath(t)
	vars := map[string]string{
		"email":    "dup@example.com",
		"password": "initial password 12",
	}
	if _, _, code := runPsqlScript(t,
		cfg.PostgresHost, cfg.PostgresUser, cfg.PostgresDB, cfg.PostgresPassword,
		script, cfg.PostgresPort, vars,
	); code != 0 {
		t.Fatalf("first create-user must succeed; code=%d", code)
	}

	// Same email a second time — must error and roll back the tx (no orphan
	// teams/memberships from the doomed INSERT chain).
	stdout, stderr, code := runPsqlScript(t,
		cfg.PostgresHost, cfg.PostgresUser, cfg.PostgresDB, cfg.PostgresPassword,
		script, cfg.PostgresPort, vars,
	)
	if code == 0 {
		t.Errorf("duplicate email exited 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "email already exists") {
		t.Errorf("expected duplicate-email error in stderr; got %q", stderr)
	}

	// Exactly one team exists (the one created by the first invocation).
	svc := auth.NewService(pool)
	dbUser, err := svc.Queries().GetUserByEmail(context.Background(), "dup@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	teams, err := svc.Queries().ListTeamsForUser(context.Background(), dbUser.ID)
	if err != nil {
		t.Fatalf("list teams: %v", err)
	}
	if len(teams) != 1 {
		t.Errorf("expected 1 team after failed duplicate, got %d (tx didn't roll back)", len(teams))
	}
}

func TestCreateUser_shortPassword_exitsNonZero(t *testing.T) {
	_, cfg := testhelpers.StartPostgres(t)
	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	if err := mmigrate.Run(context.Background(), "pg", drv, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	script := createUserScriptPath(t)
	stdout, stderr, code := runPsqlScript(t,
		cfg.PostgresHost, cfg.PostgresUser, cfg.PostgresDB, cfg.PostgresPassword,
		script, cfg.PostgresPort,
		map[string]string{
			"email":    "short@example.com",
			"password": "tooshort",
		},
	)
	if code == 0 {
		t.Errorf("short password exited 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "at least 12 characters") {
		t.Errorf("expected length error; stderr=%q", stderr)
	}
}

func TestCreateUser_emptyEmail_exitsNonZero(t *testing.T) {
	_, cfg := testhelpers.StartPostgres(t)
	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	if err := mmigrate.Run(context.Background(), "pg", drv, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	script := createUserScriptPath(t)
	stdout, stderr, code := runPsqlScript(t,
		cfg.PostgresHost, cfg.PostgresUser, cfg.PostgresDB, cfg.PostgresPassword,
		script, cfg.PostgresPort,
		map[string]string{
			"email":    "",
			"password": "initial password 12",
		},
	)
	if code == 0 {
		t.Errorf("empty email exited 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "email=... is required") {
		t.Errorf("expected email-required error; stderr=%q", stderr)
	}
}
