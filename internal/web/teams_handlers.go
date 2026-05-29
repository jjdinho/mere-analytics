package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/views"
)

// getTeam renders /teams/:id — team settings, member list, invite generation.
// All access goes through the viewer; missing membership → 404.
func getTeam(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		teamID := r.PathValue("id")
		v := auth.ViewerFrom(r.Context())
		sess := auth.SessionFrom(r.Context())

		team, err := v.Teams(r.Context()).ByID(teamID)
		if errors.Is(err, auth.ErrNotVisible) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			logger.Error("team get", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		members, err := v.Teams(r.Context()).MembersOf(teamID)
		if errors.Is(err, auth.ErrNotVisible) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			logger.Error("team members", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		renderTeamShow(w, r, sess, views.TeamShowData{Team: team, Members: members})
	}
}

// postTeamInvites issues a new invite for the team. The plaintext URL is
// rendered on this response only — render-on-POST (Issue 3 applied to
// invites for the same exactly-once UX).
func postTeamInvites(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		teamID := r.PathValue("id")
		v := auth.ViewerFrom(r.Context())
		sess := auth.SessionFrom(r.Context())

		team, err := v.Teams(r.Context()).ByID(teamID)
		if errors.Is(err, auth.ErrNotVisible) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			logger.Error("team invite: lookup team", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		result, err := v.CreateInvite(r.Context(), teamID, nowOrSvcNow(r))
		if errors.Is(err, auth.ErrNotVisible) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			logger.Error("team invite: create", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		members, err := v.Teams(r.Context()).MembersOf(teamID)
		if err != nil {
			logger.Error("team invite: members", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		inviteURL := absoluteURL(r, "/invites/"+result.Plaintext)
		renderTeamShow(w, r, sess, views.TeamShowData{
			Team:      team,
			Members:   members,
			NewInvite: inviteURL,
		})
	}
}

// postTeamProjects creates a project under teamID. Validates name; redirects
// to the new project's detail page on success.
func postTeamProjects(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		teamID := r.PathValue("id")
		v := auth.ViewerFrom(r.Context())

		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		name, err := auth.ValidateName("Project", r.PostForm.Get("name"))
		if err != nil {
			// Re-render the home page is overkill for this; redirect with a
			// 303 and rely on browsers to handle missing name. For now,
			// surface a 400 with the message.
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		proj, err := v.Projects(r.Context()).Create(teamID, name)
		if errors.Is(err, auth.ErrNotVisible) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			logger.Error("project create", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/projects/"+proj.ID, http.StatusSeeOther)
	}
}

func renderTeamShow(w http.ResponseWriter, r *http.Request, sess *auth.Session, data views.TeamShowData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.TeamShow(sess, data).Render(r.Context(), w)
}
