package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/views"
)

// getProject renders /projects/:id — project detail with the public ingest
// token and the danger-zone delete form. /v1/* + /mcp bearer auth is served
// by OAuth (no per-project token issuance UI here anymore).
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
		renderProjectShow(w, r, sess, views.ProjectShowData{
			Project:     proj,
			PublicToken: publicToken,
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

func renderProjectShow(w http.ResponseWriter, r *http.Request, sess *auth.Session, data views.ProjectShowData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.ProjectShow(sess, data).Render(r.Context(), w)
}
