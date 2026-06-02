package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/query"
	"github.com/jjdinho/mere-analytics/internal/views"
)

const playgroundMaxRows = 1000

type queryRequest struct {
	SQL string `json:"sql"`
}

func postAPIProjectQuery(authSvc *auth.Service, exec *query.Executor, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		ok, err := bearerCanReadProject(r, authSvc, projectID)
		if err != nil {
			logger.Error("api query authorize", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}

		body, decoded := DecodeJSON[queryRequest](w, r)
		if !decoded {
			return
		}
		sw := &streamingResponseWriter{ResponseWriter: w}
		sw.Header().Set("Content-Type", "application/json")
		if _, err := exec.StreamJSON(r.Context(), projectID, body.SQL, sw); err != nil {
			writeQueryError(sw, err, logger)
			return
		}
	}
}

func getAPIProjectSchema(authSvc *auth.Service, schema *query.SchemaProvider, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		ok, err := bearerCanReadProject(r, authSvc, projectID)
		if err != nil {
			logger.Error("api schema authorize", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		catalog, err := schema.Schema(r.Context())
		if err != nil {
			logger.Error("api schema", "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(catalog)
	}
}

func bearerCanReadProject(r *http.Request, authSvc *auth.Service, projectID string) (bool, error) {
	ac := oauth.FromContext(r.Context())
	if ac == nil || ac.ProjectID != projectID {
		return false, nil
	}
	v := auth.NewViewer(authSvc, ac.UserID)
	if _, err := v.Projects(r.Context()).ByID(projectID); err != nil {
		if errors.Is(err, auth.ErrNotVisible) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func getProjectQuery(schema *query.SchemaProvider, logger *slog.Logger) http.HandlerFunc {
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
			logger.Error("project query get", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		catalog, err := schema.Schema(r.Context())
		if err != nil {
			logger.Error("project query schema", "err", err)
			renderProjectQuery(w, r, sess, views.ProjectQueryData{Project: proj, PageTitle: proj.Name + " query", ErrMsg: err.Error()})
			return
		}
		renderProjectQuery(w, r, sess, views.ProjectQueryData{
			Project:   proj,
			PageTitle: proj.Name + " query",
			SQL:       "SELECT timestamp, event, distinct_id, properties FROM events ORDER BY timestamp DESC LIMIT 100",
			Schema:    catalog,
		})
	}
}

func postProjectQuery(exec *query.Executor, schema *query.SchemaProvider, logger *slog.Logger) http.HandlerFunc {
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
			logger.Error("project query authorize", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		sqlText := strings.TrimSpace(r.PostForm.Get("sql"))
		catalog, err := schema.Schema(r.Context())
		if err != nil {
			logger.Error("project query schema", "err", err)
			renderProjectQuery(w, r, sess, views.ProjectQueryData{Project: proj, PageTitle: proj.Name + " query", SQL: sqlText, ErrMsg: err.Error()})
			return
		}
		result, err := exec.Collect(r.Context(), projectID, sqlText, playgroundMaxRows)
		data := views.ProjectQueryData{Project: proj, PageTitle: proj.Name + " query", SQL: sqlText, Schema: catalog, Result: result}
		if err != nil {
			if errors.Is(err, query.ErrRowLimitExceeded) {
				data.ErrMsg = "Query returned more than 1000 rows. Add a LIMIT and run it again."
			} else {
				data.ErrMsg = queryErrorMessage(err)
				logger.Warn("project query run", "err", err)
			}
		}
		renderProjectQuery(w, r, sess, data)
	}
}

func renderProjectQuery(w http.ResponseWriter, r *http.Request, sess *auth.Session, data views.ProjectQueryData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.ProjectQuery(sess, data).Render(r.Context(), w)
}

type streamingResponseWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *streamingResponseWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *streamingResponseWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func writeQueryError(w *streamingResponseWriter, err error, logger *slog.Logger) {
	if w.wrote {
		logger.Warn("query stream failed after response began", "err", err)
		return
	}
	http.Error(w, queryErrorMessage(err), http.StatusBadRequest)
}

func queryErrorMessage(err error) string {
	if errors.Is(err, query.ErrEmptySQL) {
		return "sql is required"
	}
	msg := err.Error()
	return strings.TrimPrefix(msg, "clickhouse query: ")
}
