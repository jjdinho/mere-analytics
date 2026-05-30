package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jjdinho/mere-analytics/internal/auth"
)

// TestConsumeInvite_HappyPath: alice issues an invite for her personal team;
// bob (different user) consumes it and ends up a member.
func TestConsumeInvite_HappyPath(t *testing.T) {
	svc := startService(t)
	aliceID, aliceTeamID := signupForTest(t, svc, "alice@example.com")
	bobID, _ := signupForTest(t, svc, "bob@example.com")
	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	res, err := vAlice.CreateInvite(ctx, aliceTeamID, time.Now())
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if res.Plaintext == res.Invite.TokenHash {
		t.Errorf("plaintext stored as hash: %s", res.Plaintext)
	}

	team, err := svc.ConsumeInvite(ctx, bobID, res.Plaintext)
	if err != nil {
		t.Fatalf("consume invite: %v", err)
	}
	if team.ID != aliceTeamID {
		t.Errorf("returned team id: got %s want %s", team.ID, aliceTeamID)
	}

	// Bob is now a member.
	vBob := auth.NewViewer(svc, bobID)
	got, err := vBob.Teams(ctx).ByID(aliceTeamID)
	if err != nil {
		t.Fatalf("bob view alice team: %v", err)
	}
	if got.ID != aliceTeamID {
		t.Errorf("got %s want %s", got.ID, aliceTeamID)
	}
}

func TestConsumeInvite_InvalidToken(t *testing.T) {
	svc := startService(t)
	_, _ = signupForTest(t, svc, "alice@example.com")
	bobID, _ := signupForTest(t, svc, "bob@example.com")

	_, err := svc.ConsumeInvite(context.Background(), bobID, "mere_pat_no_such_invite_hash_collision_unlikely_xyz")
	if !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("unknown invite: got %v want ErrInviteInvalid", err)
	}
}

func TestConsumeInvite_AlreadyConsumed(t *testing.T) {
	svc := startService(t)
	aliceID, aliceTeamID := signupForTest(t, svc, "alice@example.com")
	bobID, _ := signupForTest(t, svc, "bob@example.com")
	carolID, _ := signupForTest(t, svc, "carol@example.com")
	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	res, err := vAlice.CreateInvite(ctx, aliceTeamID, time.Now())
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, err := svc.ConsumeInvite(ctx, bobID, res.Plaintext); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	_, err = svc.ConsumeInvite(ctx, carolID, res.Plaintext)
	if !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("second consume: got %v want ErrInviteInvalid", err)
	}
}

// TestConsumeInvite_AlreadyMember verifies Issue 10: if the consuming user
// is already in the team, the invite is still burned (single-use semantic)
// and the call returns success.
func TestConsumeInvite_AlreadyMember(t *testing.T) {
	svc := startService(t)
	aliceID, aliceTeamID := signupForTest(t, svc, "alice@example.com")
	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	res, _ := vAlice.CreateInvite(ctx, aliceTeamID, time.Now())

	// Alice consumes her own invite — already a member; should succeed.
	team, err := svc.ConsumeInvite(ctx, aliceID, res.Plaintext)
	if err != nil {
		t.Fatalf("self-consume: %v", err)
	}
	if team.ID != aliceTeamID {
		t.Errorf("got %s want %s", team.ID, aliceTeamID)
	}

	// And the invite is burned — a second consume by anyone fails.
	bobID, _ := signupForTest(t, svc, "bob@example.com")
	_, err = svc.ConsumeInvite(ctx, bobID, res.Plaintext)
	if !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("after self-consume: bob should get ErrInviteInvalid, got %v", err)
	}
}

// TestConsumeInvite_RaceOnlyOneWins is the plan-required concurrency test
// (Issue 14): two distinct users hit ConsumeInvite at the same time;
// exactly one wins.
func TestConsumeInvite_RaceOnlyOneWins(t *testing.T) {
	svc := startService(t)
	aliceID, aliceTeamID := signupForTest(t, svc, "alice@example.com")
	bobID, _ := signupForTest(t, svc, "bob@example.com")
	carolID, _ := signupForTest(t, svc, "carol@example.com")
	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	res, _ := vAlice.CreateInvite(ctx, aliceTeamID, time.Now())

	var (
		mu      sync.Mutex
		winners []string
		losers  int
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, uid := range []string{bobID, carolID} {
		wg.Add(1)
		go func(userID string) {
			defer wg.Done()
			<-start
			_, err := svc.ConsumeInvite(ctx, userID, res.Plaintext)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, userID)
			case errors.Is(err, auth.ErrInviteInvalid):
				losers++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(uid)
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Errorf("winners: got %d want 1 (%v)", len(winners), winners)
	}
	if losers != 1 {
		t.Errorf("losers: got %d want 1", losers)
	}
}

// TestSignupWithInvite_StrictOnInvalid verifies Issue 12: a malformed/missing
// invite passed via SignupWithInvite aborts the entire signup transaction.
func TestSignupWithInvite_StrictOnInvalid(t *testing.T) {
	svc := startService(t)
	ctx := context.Background()

	_, err := svc.SignupWithInvite(ctx, auth.SignupRequest{
		Email:    "newbie@example.com",
		Password: "correct horse battery staple",
	}, "mere_pat_garbage_no_such_invite_token_zzzz_xxx_yyy_ww")
	if !errors.Is(err, auth.ErrInviteInvalid) {
		t.Errorf("got %v want ErrInviteInvalid", err)
	}
	// And no user / team rows landed.
	if _, err := svc.Authenticate(ctx, "newbie@example.com", "correct horse battery staple"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("signup tx should have rolled back; got %v want ErrInvalidCredentials", err)
	}
}

func TestSignupWithInvite_HappyPath(t *testing.T) {
	svc := startService(t)
	aliceID, aliceTeamID := signupForTest(t, svc, "alice@example.com")
	ctx := context.Background()

	vAlice := auth.NewViewer(svc, aliceID)
	res, _ := vAlice.CreateInvite(ctx, aliceTeamID, time.Now())

	result, err := svc.SignupWithInvite(ctx, auth.SignupRequest{
		Email:    "newbie@example.com",
		Password: "correct horse battery staple",
	}, res.Plaintext)
	if err != nil {
		t.Fatalf("signup with invite: %v", err)
	}

	// New user is in both the personal team AND alice's team.
	vNew := auth.NewViewer(svc, result.User.ID)
	teams, err := vNew.Teams(ctx).List()
	if err != nil {
		t.Fatalf("list teams: %v", err)
	}
	if len(teams) != 2 {
		t.Errorf("team count: got %d want 2", len(teams))
	}
	// And the invite is consumed.
	_, err = svc.Queries().GetActiveInviteByHash(ctx, auth.HashToken(res.Plaintext))
	if err == nil {
		t.Errorf("invite should be consumed (no longer active)")
	}
}

// TestChangePassword covers the basic happy + sad paths used by /account/password.
func TestChangePassword(t *testing.T) {
	svc := startService(t)
	aliceID, _ := signupForTest(t, svc, "alice@example.com")
	ctx := context.Background()

	// Wrong current password.
	err := svc.ChangePassword(ctx, aliceID, "wrong", "new correct horse battery")
	if !errors.Is(err, auth.ErrCurrentPasswordWrong) {
		t.Errorf("wrong current: got %v want ErrCurrentPasswordWrong", err)
	}

	// Short new password → ValidationError.
	err = svc.ChangePassword(ctx, aliceID, "correct horse battery staple", "short")
	var ve *auth.ValidationError
	if !errors.As(err, &ve) {
		t.Errorf("short new password: got %v want *ValidationError", err)
	}

	// Happy path.
	if err := svc.ChangePassword(ctx, aliceID, "correct horse battery staple", "new correct horse battery"); err != nil {
		t.Errorf("happy: %v", err)
	}
	// Old password no longer works; new password does.
	if _, err := svc.Authenticate(ctx, "alice@example.com", "correct horse battery staple"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("old password should no longer work")
	}
	if _, err := svc.Authenticate(ctx, "alice@example.com", "new correct horse battery"); err != nil {
		t.Errorf("new password should work: %v", err)
	}
}
