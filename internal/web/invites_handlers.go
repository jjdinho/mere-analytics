package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
	"github.com/jjdinho/mere-analytics/internal/views"
)

// getInvite renders the confirmation page for /invites/:token. Public route
// (anonymous and authenticated both flow through). The actual join is a
// separate POST that mutates state (Issue 9 — defends against CSRF auto-
// join via cross-site link).
//
//   anon       → inline signup form ("create account and join") + link to /login?invite=:t
//   auth+!mbr  → "Click below to add yourself" + POST form
//   auth+mbr   → "You're already a member" + POST form (which silent-burns)
//   invalid    → 404 page (consumed / expired / unknown)
func getInvite(svc *auth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokenPlain := r.PathValue("token")
		sess := auth.SessionFrom(r.Context())

		row, err := svc.Queries().GetActiveInviteByHash(r.Context(), auth.HashToken(tokenPlain))
		if errors.Is(err, pgx.ErrNoRows) {
			renderInviteInvalid(w, r, sess)
			return
		}
		if err != nil {
			logger.Error("invite get", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		var isMember bool
		if sess != nil {
			m, merr := svc.Queries().IsMemberOfTeam(r.Context(), db.IsMemberOfTeamParams{
				TeamID: row.TeamID,
				UserID: sess.UserID,
			})
			if merr != nil {
				logger.Error("invite membership check", "err", merr)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			isMember = m
		}

		data := views.InviteConfirmData{
			TeamName:      row.TeamName,
			PageTitle:     "Join " + row.TeamName,
			InviteToken:   tokenPlain,
			AlreadyMember: isMember,
			AnonLoginURL:  "/login?invite=" + tokenPlain,
		}
		renderInviteConfirm(w, r, sess, data)
	}
}

// postInvite handles both the authenticated and anonymous paths for
// /invites/:token POST.
//
//   anon  → SignupWithInvite creates the user, personal team, membership,
//           and consumes the invite in one transaction; we then issue a
//           session and land the user on the invited team page.
//   auth  → ConsumeInvite (atomic UPDATE + ON CONFLICT DO NOTHING
//           membership insert). Already-a-member callers get silent success
//           (Issue 10). Concurrent claims race exactly once (Issue 14).
func postInvite(svc *auth.Service, logger *slog.Logger, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokenPlain := r.PathValue("token")
		sess := auth.SessionFrom(r.Context())
		if sess == nil {
			handleAnonInvitePost(w, r, svc, logger, tokenPlain, secureCookies)
			return
		}

		team, err := svc.ConsumeInvite(r.Context(), sess.UserID, tokenPlain)
		if errors.Is(err, auth.ErrInviteInvalid) {
			renderInviteInvalid(w, r, sess)
			return
		}
		if err != nil {
			logger.Error("invite consume", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/teams/"+team.ID, http.StatusSeeOther)
	}
}

// handleAnonInvitePost runs the create-account-and-join flow for a
// logged-out invitee. On error it re-renders the InviteConfirm page (with
// the team name re-fetched) so the form keeps its context.
func handleAnonInvitePost(w http.ResponseWriter, r *http.Request, svc *auth.Service, logger *slog.Logger, tokenPlain string, secureCookies bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	email := r.PostForm.Get("email")
	password := r.PostForm.Get("password")

	res, err := svc.SignupWithInvite(r.Context(), auth.SignupRequest{Email: email, Password: password}, tokenPlain)
	switch {
	case errors.Is(err, auth.ErrInviteInvalid):
		// Token went stale between GET and POST, or was malformed.
		renderInviteInvalid(w, r, nil)
		return
	case errors.Is(err, auth.ErrEmailTaken):
		w.WriteHeader(http.StatusConflict)
		renderAnonInviteError(w, r, svc, logger, tokenPlain, email, "Email already registered — log in below to join this team.")
		return
	case err != nil:
		var ve *auth.ValidationError
		if errors.As(err, &ve) {
			w.WriteHeader(http.StatusBadRequest)
			renderAnonInviteError(w, r, svc, logger, tokenPlain, email, ve.Msg)
			return
		}
		logger.Error("anon invite signup failed", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	sess, err := svc.CreateSession(r.Context(), res.User.ID)
	if err != nil {
		logger.Error("session create after invite signup failed", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, sess.ID, sess.ExpiresAt, secureCookies)
	http.Redirect(w, r, "/teams/"+res.InvitedTeam.ID, http.StatusSeeOther)
}

// renderAnonInviteError re-fetches the invite (still valid at this point —
// errors that imply otherwise are handled above) so the page can show the
// team name alongside the error banner.
func renderAnonInviteError(w http.ResponseWriter, r *http.Request, svc *auth.Service, logger *slog.Logger, tokenPlain, email, errMsg string) {
	row, err := svc.Queries().GetActiveInviteByHash(r.Context(), auth.HashToken(tokenPlain))
	if errors.Is(err, pgx.ErrNoRows) {
		renderInviteInvalid(w, r, nil)
		return
	}
	if err != nil {
		logger.Error("invite re-fetch on error", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	renderInviteConfirm(w, r, nil, views.InviteConfirmData{
		TeamName:     row.TeamName,
		PageTitle:    "Join " + row.TeamName,
		InviteToken:  tokenPlain,
		AnonLoginURL: "/login?invite=" + tokenPlain,
		Email:        email,
		ErrMsg:       errMsg,
	})
}

func renderInviteConfirm(w http.ResponseWriter, r *http.Request, sess *auth.Session, data views.InviteConfirmData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.InviteConfirm(sess, data).Render(r.Context(), w)
}

func renderInviteInvalid(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = views.InviteInvalid(sess).Render(r.Context(), w)
}
