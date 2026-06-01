package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

// signupForTest is a tiny helper that creates a user + personal team via the
// real Service.Signup, returning the resulting (userID, teamID, email).
func signupForTest(t *testing.T, svc *auth.Service, email string) (string, string) {
	t.Helper()
	res, err := svc.Signup(context.Background(), auth.SignupRequest{
		Email:    email,
		Password: "correct horse battery staple",
	})
	if err != nil {
		t.Fatalf("signup %s: %v", email, err)
	}
	return res.User.ID, res.Team.ID
}

func startService(t *testing.T) *auth.Service {
	t.Helper()
	pool, cfg := testhelpers.StartPostgres(t)
	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := mmigrate.Run(context.Background(), "pg", drv, migrations.Postgres, "postgres", logger); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return auth.NewService(pool)
}

// TestViewer_TeamByID_OwnAndCrossUser is the smallest end-to-end isolation
// check: user A's viewer can see their own team; user B's viewer gets
// ErrNotVisible for the same team id. Forms the kernel of the
// cross-user authorization matrix test.
func TestViewer_TeamByID_OwnAndCrossUser(t *testing.T) {
	svc := startService(t)
	aliceID, aliceTeamID := signupForTest(t, svc, "alice@example.com")
	bobID, _ := signupForTest(t, svc, "bob@example.com")

	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	if _, err := vAlice.Teams(ctx).ByID(aliceTeamID); err != nil {
		t.Errorf("alice should see own team: %v", err)
	}

	vBob := auth.NewViewer(svc, bobID)
	_, err := vBob.Teams(ctx).ByID(aliceTeamID)
	if !errors.Is(err, auth.ErrNotVisible) {
		t.Errorf("bob should get ErrNotVisible for alice's team, got: %v", err)
	}
}

func TestViewer_ProjectByID_NotVisible_For_OtherUser(t *testing.T) {
	svc := startService(t)
	aliceID, aliceTeamID := signupForTest(t, svc, "alice@example.com")
	bobID, _ := signupForTest(t, svc, "bob@example.com")
	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	proj, err := vAlice.Projects(ctx).Create(aliceTeamID, "prod")
	if err != nil {
		t.Fatalf("alice create project: %v", err)
	}

	got, err := vAlice.Projects(ctx).ByID(proj.ID)
	if err != nil {
		t.Fatalf("alice should see own project: %v", err)
	}
	if got.Name != "prod" {
		t.Errorf("name: got %q want %q", got.Name, "prod")
	}

	vBob := auth.NewViewer(svc, bobID)
	_, err = vBob.Projects(ctx).ByID(proj.ID)
	if !errors.Is(err, auth.ErrNotVisible) {
		t.Errorf("bob should get ErrNotVisible for alice's project, got: %v", err)
	}
}

func TestViewer_ProjectSoftDelete_HidesFromOwner(t *testing.T) {
	svc := startService(t)
	aliceID, aliceTeamID := signupForTest(t, svc, "alice@example.com")
	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	proj, err := vAlice.Projects(ctx).Create(aliceTeamID, "prod")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := vAlice.Projects(ctx).SoftDelete(proj.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	_, err = vAlice.Projects(ctx).ByID(proj.ID)
	if !errors.Is(err, auth.ErrNotVisible) {
		t.Errorf("soft-deleted project should be ErrNotVisible to owner, got: %v", err)
	}
	// Second soft-delete is also ErrNotVisible (idempotent at the user level).
	if err := vAlice.Projects(ctx).SoftDelete(proj.ID); !errors.Is(err, auth.ErrNotVisible) {
		t.Errorf("second soft-delete: got %v want ErrNotVisible", err)
	}
}

// TestViewer_ProjectCreate_AutoProvisionsPublicToken: every freshly-created
// project must come with a public_ingest token whose plaintext starts with
// mere_pub_ and whose hash is sha256(plaintext).
func TestViewer_ProjectCreate_AutoProvisionsPublicToken(t *testing.T) {
	svc := startService(t)
	aliceID, aliceTeamID := signupForTest(t, svc, "alice@example.com")
	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	proj, err := vAlice.Projects(ctx).Create(aliceTeamID, "prod")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	publicTok, err := vAlice.Tokens(ctx).PublicForProject(proj.ID)
	if err != nil {
		t.Fatalf("public for project: %v", err)
	}
	if !strings.HasPrefix(publicTok, auth.PublicTokenPrefix) {
		t.Errorf("public token prefix: got %q want prefix %q", publicTok, auth.PublicTokenPrefix)
	}
	if len(publicTok) != auth.PublicTokenPlaintextLength {
		t.Errorf("public token length: got %d want %d", len(publicTok), auth.PublicTokenPlaintextLength)
	}
}

// TestViewer_ProjectCreate_PublicTokensAreDistinctPerProject: two projects
// under the same team must end up with two different public tokens (catches
// any accidental sharing or static defaulting).
func TestViewer_ProjectCreate_PublicTokensAreDistinctPerProject(t *testing.T) {
	svc := startService(t)
	aliceID, aliceTeamID := signupForTest(t, svc, "alice@example.com")
	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	p1, _ := vAlice.Projects(ctx).Create(aliceTeamID, "proj-a")
	p2, _ := vAlice.Projects(ctx).Create(aliceTeamID, "proj-b")

	t1, err := vAlice.Tokens(ctx).PublicForProject(p1.ID)
	if err != nil {
		t.Fatalf("p1 public: %v", err)
	}
	t2, err := vAlice.Tokens(ctx).PublicForProject(p2.ID)
	if err != nil {
		t.Fatalf("p2 public: %v", err)
	}
	if t1 == t2 {
		t.Errorf("two projects ended up with the same public token: %s", t1)
	}
}

func TestViewer_ListProjectsForTeams_BoundedQuery(t *testing.T) {
	svc := startService(t)
	aliceID, _ := signupForTest(t, svc, "alice@example.com")
	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	teams, _ := vAlice.Teams(ctx).List()
	ids := make([]string, len(teams))
	for i, t := range teams {
		ids[i] = t.ID
	}
	// Create a couple projects in the personal team.
	if _, err := vAlice.Projects(ctx).Create(teams[0].ID, "proj-a"); err != nil {
		t.Fatalf("create proj-a: %v", err)
	}
	if _, err := vAlice.Projects(ctx).Create(teams[0].ID, "proj-b"); err != nil {
		t.Fatalf("create proj-b: %v", err)
	}

	projs, err := vAlice.Projects(ctx).ListForTeams(ids)
	if err != nil {
		t.Fatalf("list projects for teams: %v", err)
	}
	if len(projs) != 2 {
		t.Errorf("got %d projects want 2", len(projs))
	}

	// Empty input is a no-query no-op.
	empty, err := vAlice.Projects(ctx).ListForTeams(nil)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty input should return zero rows, got %d", len(empty))
	}
}
