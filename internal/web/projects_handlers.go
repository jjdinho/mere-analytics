package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/views"
)

// getProject renders /projects/:id — project detail with the active token
// list, token-create form, and the danger-zone delete form. Tokens are
// reloaded on every render so a freshly-revoked one disappears immediately.
func getProject(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("id")
		v := auth.ViewerFrom(r.Context())
		sess := auth.SessionFrom(r.Context())

		proj, err := v.Projects(r.Context()).ByID(projectID)
		if errors.Is(err, auth.ErrNotVisible) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			logger.Error("project get", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		publicToken, err := v.Tokens(r.Context()).PublicForProject(projectID)
		if err != nil {
			logger.Error("project public token fetch", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		tokens, err := v.Tokens(r.Context()).ListForProject(projectID)
		if err != nil {
			logger.Error("project tokens list", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		renderProjectShow(w, r, sess, views.ProjectShowData{
			Project:     proj,
			PublicToken: publicToken,
			Tokens:      tokens,
		})
	}
}

// postProjectDelete soft-deletes the project. Subsequent GETs on it 404 (the
// viewer's SELECT filters deleted_at IS NULL). Redirects to home.
func postProjectDelete(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("id")
		v := auth.ViewerFrom(r.Context())

		if err := v.Projects(r.Context()).SoftDelete(projectID); err != nil {
			if errors.Is(err, auth.ErrNotVisible) {
				http.NotFound(w, r)
				return
			}
			logger.Error("project soft delete", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// postProjectTokens issues a new API token for the project. Plaintext is
// rendered into this response only — never echoed by the token list page
// (Issue 3 render-on-POST semantics).
func postProjectTokens(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("id")
		v := auth.ViewerFrom(r.Context())
		sess := auth.SessionFrom(r.Context())

		proj, err := v.Projects(r.Context()).ByID(projectID)
		if errors.Is(err, auth.ErrNotVisible) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			logger.Error("token create: lookup project", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		name, err := auth.ValidateName("Token", r.PostForm.Get("name"))
		if err != nil {
			publicToken, perr := v.Tokens(r.Context()).PublicForProject(projectID)
			if perr != nil {
				logger.Error("token create: public fetch after validation", "err", perr)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			tokens, terr := v.Tokens(r.Context()).ListForProject(projectID)
			if terr != nil {
				logger.Error("token create: list after validation", "err", terr)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			renderProjectShow(w, r, sess, views.ProjectShowData{
				Project:     proj,
				PublicToken: publicToken,
				Tokens:      tokens,
				ErrMsg:      err.Error(),
			})
			return
		}

		result, err := v.Tokens(r.Context()).Create(projectID, name)
		if errors.Is(err, auth.ErrNotVisible) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			logger.Error("token create", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		publicToken, err := v.Tokens(r.Context()).PublicForProject(projectID)
		if err != nil {
			logger.Error("token create: public fetch after create", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		tokens, err := v.Tokens(r.Context()).ListForProject(projectID)
		if err != nil {
			logger.Error("token create: list after create", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		renderProjectShow(w, r, sess, views.ProjectShowData{
			Project:     proj,
			PublicToken: publicToken,
			Tokens:      tokens,
			NewToken:    result.Plaintext,
		})
	}
}

// postProjectTokenRevoke flips the revoked_at on the named token. Idempotent
// — a second revoke returns 404 (RowsAffected == 0 → ErrNotVisible). On
// success, redirects to the project detail so the list reflects the change.
func postProjectTokenRevoke(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("id")
		tokenID := r.PathValue("tid")
		v := auth.ViewerFrom(r.Context())

		if err := v.Tokens(r.Context()).Revoke(projectID, tokenID); err != nil {
			if errors.Is(err, auth.ErrNotVisible) {
				http.NotFound(w, r)
				return
			}
			logger.Error("token revoke", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/projects/"+projectID, http.StatusSeeOther)
	}
}

func renderProjectShow(w http.ResponseWriter, r *http.Request, sess *auth.Session, data views.ProjectShowData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.ProjectShow(sess, data).Render(r.Context(), w)
}
