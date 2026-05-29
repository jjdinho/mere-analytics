package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/views"
)

// indexHandler dispatches between the public landing and the logged-in home
// based on whether authMiddleware found a session for the request.
func indexHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if s := auth.SessionFrom(r.Context()); s != nil {
			_ = views.Home(s).Render(r.Context(), w)
			return
		}
		_ = views.Index().Render(r.Context(), w)
	}
}

// getSignup renders the signup form.
func getSignup() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderSignup(w, r, "", "")
	}
}

// postSignup creates a user + team + membership in one transaction and logs
// the new account in. CSRF was already verified by authMiddleware.
func postSignup(svc *auth.Service, logger *slog.Logger, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		email := r.PostForm.Get("email")
		password := r.PostForm.Get("password")

		res, err := svc.Signup(r.Context(), auth.SignupRequest{Email: email, Password: password})
		switch {
		case errors.Is(err, auth.ErrEmailTaken):
			w.WriteHeader(http.StatusConflict)
			renderSignup(w, r, email, "Email already registered.")
			return
		case err != nil:
			var ve *auth.ValidationError
			if errors.As(err, &ve) {
				w.WriteHeader(http.StatusBadRequest)
				renderSignup(w, r, email, ve.Msg)
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

func getLogin() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderLogin(w, r, "", "")
	}
}

func postLogin(svc *auth.Service, logger *slog.Logger, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		email := r.PostForm.Get("email")
		password := r.PostForm.Get("password")

		user, err := svc.Authenticate(r.Context(), email, password)
		if errors.Is(err, auth.ErrInvalidCredentials) {
			w.WriteHeader(http.StatusUnauthorized)
			renderLogin(w, r, email, "Invalid credentials.")
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

func renderSignup(w http.ResponseWriter, r *http.Request, email, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.Signup(email, errMsg).Render(r.Context(), w)
}

func renderLogin(w http.ResponseWriter, r *http.Request, email, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.Login(email, errMsg).Render(r.Context(), w)
}

