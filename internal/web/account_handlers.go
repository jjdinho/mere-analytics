package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/views"
)

// getAccountPassword renders the change-password form. The banner is shown
// only when session.MustChangePassword is set (the operator-reset path).
func getAccountPassword() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := auth.SessionFrom(r.Context())
		renderAccountPassword(w, r, sess, views.AccountPasswordData{Forced: sess.MustChangePassword})
	}
}

// postAccountPassword validates + applies the password change. On success
// the must_change_password flag is cleared (in svc.ChangePassword); the user
// is redirected to home — the next request will pass the middleware gate.
func postAccountPassword(svc *auth.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := auth.SessionFrom(r.Context())
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		current := r.PostForm.Get("current_password")
		newpw := r.PostForm.Get("new_password")

		err := svc.ChangePassword(r.Context(), sess.UserID, current, newpw)
		switch {
		case errors.Is(err, auth.ErrCurrentPasswordWrong):
			w.WriteHeader(http.StatusBadRequest)
			renderAccountPassword(w, r, sess, views.AccountPasswordData{
				Forced: sess.MustChangePassword,
				ErrMsg: "Current password is incorrect.",
			})
			return
		case err != nil:
			var ve *auth.ValidationError
			if errors.As(err, &ve) {
				w.WriteHeader(http.StatusBadRequest)
				renderAccountPassword(w, r, sess, views.AccountPasswordData{
					Forced: sess.MustChangePassword,
					ErrMsg: ve.Msg,
				})
				return
			}
			logger.Error("change password", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		// Refresh the session row's view of MustChangePassword so the next
		// request after this redirect doesn't bounce back. Cheapest path:
		// update the in-memory copy; the next session lookup re-reads PG
		// regardless.
		sess.MustChangePassword = false
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func renderAccountPassword(w http.ResponseWriter, r *http.Request, sess *auth.Session, data views.AccountPasswordData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.AccountPassword(sess, data).Render(r.Context(), w)
}
