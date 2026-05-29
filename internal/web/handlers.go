package web

import (
	"errors"
	"log/slog"
	"net/http"
	"sort"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
	"github.com/jjdinho/mere-analytics/internal/views"
)

// indexHandler dispatches between the public landing and the logged-in home
// based on whether authMiddleware found a session for the request.
//
// Logged-in path runs the bounded 2-query home pattern (Issue 15): list the
// viewer's teams, then projects across all those team ids in one trip.
func indexHandler(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		sess := auth.SessionFrom(r.Context())
		if sess == nil {
			_ = views.Index().Render(r.Context(), w)
			return
		}
		v := auth.ViewerFrom(r.Context())
		teams, err := v.Teams(r.Context()).List()
		if err != nil {
			logger.Error("home: list teams", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		ids := make([]string, len(teams))
		for i, t := range teams {
			ids[i] = t.ID
		}
		projects, err := v.Projects(r.Context()).ListForTeams(ids)
		if err != nil {
			logger.Error("home: list projects", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		groups := groupProjectsByTeam(teams, projects)
		_ = views.Home(sess, groups).Render(r.Context(), w)
	}
}

// groupProjectsByTeam buckets projects into HomeData entries in teams' order.
// projects come back ORDER BY team_id but we want them in the teams-as-listed
// order, so use a map then iterate teams.
func groupProjectsByTeam(teams []db.Team, projects []db.Project) []views.HomeData {
	byTeam := make(map[string][]db.Project, len(teams))
	for _, p := range projects {
		byTeam[p.TeamID] = append(byTeam[p.TeamID], p)
	}
	groups := make([]views.HomeData, 0, len(teams))
	for _, t := range teams {
		ps := byTeam[t.ID]
		sort.SliceStable(ps, func(i, j int) bool {
			return ps[i].CreatedAt.Time.Before(ps[j].CreatedAt.Time)
		})
		groups = append(groups, views.HomeData{Team: t, Projects: ps})
	}
	return groups
}

// inviteFromQuery returns the optional ?invite=:t query param, used by
// signup/login to thread an invite through the form (Issue 8). Empty string
// when absent.
func inviteFromQuery(r *http.Request) string { return r.URL.Query().Get("invite") }

// getSignup renders the signup form, threading any ?invite=:t through.
func getSignup(svc *auth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		invite := inviteFromQuery(r)
		teamName := ""
		if invite != "" {
			if row, err := svc.Queries().GetActiveInviteByHash(r.Context(), auth.HashToken(invite)); err == nil {
				teamName = row.TeamName
			}
			// If invite is invalid here, render the plain form anyway — the
			// strict check happens at POST (Issue 12).
		}
		renderSignup(w, r, "", "", invite, teamName)
	}
}

// postSignup creates a user + team + membership in one transaction. If the
// request carries an invite token (hidden field), the invite is consumed in
// the same tx and membership in the invited team is added too. Strict path
// (Issue 12): invalid invite at POST → re-render the form with the invite
// cleared and an error banner.
func postSignup(svc *auth.Service, logger *slog.Logger, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		email := r.PostForm.Get("email")
		password := r.PostForm.Get("password")
		invite := r.PostForm.Get("invite")

		res, err := svc.SignupWithInvite(r.Context(), auth.SignupRequest{Email: email, Password: password}, invite)
		switch {
		case errors.Is(err, auth.ErrEmailTaken):
			w.WriteHeader(http.StatusConflict)
			renderSignup(w, r, email, "Email already registered.", invite, "")
			return
		case errors.Is(err, auth.ErrInviteInvalid):
			// Strict: drop the invite and let the user resubmit. We don't
			// clear the password (re-rendering would echo it) — the autofocus
			// shifts the user back to the password field on most browsers.
			w.WriteHeader(http.StatusBadRequest)
			renderSignup(w, r, email, "Your invite is no longer valid. Sign up to create a personal team, or ask the inviter for a fresh link.", "", "")
			return
		case err != nil:
			var ve *auth.ValidationError
			if errors.As(err, &ve) {
				w.WriteHeader(http.StatusBadRequest)
				renderSignup(w, r, email, ve.Msg, invite, "")
				return
			}
			logger.Error("signup failed", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		sess, err := svc.CreateSession(r.Context(), res.User.ID)
		if err != nil {
			logger.Error("session create after signup failed", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, sess.ID, sess.ExpiresAt, secureCookies)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func getLogin(svc *auth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		invite := inviteFromQuery(r)
		teamName := ""
		if invite != "" {
			if row, err := svc.Queries().GetActiveInviteByHash(r.Context(), auth.HashToken(invite)); err == nil {
				teamName = row.TeamName
			}
		}
		renderLogin(w, r, "", "", invite, teamName)
	}
}

// postLogin authenticates + creates a session. When an invite token is
// supplied (hidden field) and login succeeds, the invite is consumed and
// the user is redirected to the invited team. Invalid invite at this stage
// matches signup's strict path — user is told to try again without invite.
func postLogin(svc *auth.Service, logger *slog.Logger, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		email := r.PostForm.Get("email")
		password := r.PostForm.Get("password")
		invite := r.PostForm.Get("invite")

		user, err := svc.Authenticate(r.Context(), email, password)
		if errors.Is(err, auth.ErrInvalidCredentials) {
			w.WriteHeader(http.StatusUnauthorized)
			renderLogin(w, r, email, "Invalid credentials.", invite, "")
			return
		}
		if err != nil {
			logger.Error("login failed", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		sess, err := svc.CreateSession(r.Context(), user.ID)
		if err != nil {
			logger.Error("session create after login failed", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, sess.ID, sess.ExpiresAt, secureCookies)

		if invite != "" {
			team, ierr := svc.ConsumeInvite(r.Context(), user.ID, invite)
			if errors.Is(ierr, auth.ErrInviteInvalid) {
				// Login succeeded; invite didn't. Land on home with a flash.
				// Without a persistent flash channel we simply redirect to /
				// — the user is in.
				http.Redirect(w, r, "/?invite_invalid=1", http.StatusSeeOther)
				return
			}
			if ierr != nil {
				logger.Error("login invite consume", "err", ierr)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "/teams/"+team.ID, http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// postLogout destroys the session and clears the cookie. Anonymous callers
// fall through cleanly — useful when a stale cookie has already been expired
// server-side.
func postLogout(svc *auth.Service, logger *slog.Logger, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s := auth.SessionFrom(r.Context()); s != nil {
			if err := svc.DestroySession(r.Context(), s.ID); err != nil {
				logger.Warn("session destroy failed", "err", err)
			}
		}
		clearSessionCookie(w, secureCookies)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

func renderSignup(w http.ResponseWriter, r *http.Request, email, errMsg, invitePlaintext, inviteTeamName string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.Signup(email, errMsg, invitePlaintext, inviteTeamName).Render(r.Context(), w)
}

func renderLogin(w http.ResponseWriter, r *http.Request, email, errMsg, invitePlaintext, inviteTeamName string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.Login(email, errMsg, invitePlaintext, inviteTeamName).Render(r.Context(), w)
}
