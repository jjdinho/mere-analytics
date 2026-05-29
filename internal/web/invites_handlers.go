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
//   anon       → CTA: "Sign up to join" / "Log in to join" (Issue 8)
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
			AnonSignupURL: "/signup?invite=" + tokenPlain,
			AnonLoginURL:  "/login?invite=" + tokenPlain,
		}
		renderInviteConfirm(w, r, sess, data)
	}
}

// postInvite consumes the invite for the authenticated user. The atomic
// UPDATE inside Service.ConsumeInvite ensures concurrent claims can't both
// succeed (Issue 14 race test). Already-a-member callers get silent success
// (Issue 10). Anonymous callers are redirected to /login?invite=:t — the
// requireSession middleware would already do this, but we never reach the
// handler then; this branch is defensive in case requireSession is removed.
func postInvite(svc *auth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokenPlain := r.PathValue("token")
		sess := auth.SessionFrom(r.Context())
		if sess == nil {
			http.Redirect(w, r, "/login?invite="+tokenPlain, http.StatusSeeOther)
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

func renderInviteConfirm(w http.ResponseWriter, r *http.Request, sess *auth.Session, data views.InviteConfirmData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.InviteConfirm(sess, data).Render(r.Context(), w)
}

func renderInviteInvalid(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = views.InviteInvalid(sess).Render(r.Context(), w)
}
