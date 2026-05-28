// Package web wires the application's HTTP surface. For the step 1-2 slice
// that surface is intentionally tiny: a healthcheck, a hello page, and a
// static asset mount that's currently empty but reserved for later steps.
package web

import (
	"log/slog"
	"net/http"

	"github.com/jjdinho/mere-analytics/internal/static"
	"github.com/jjdinho/mere-analytics/internal/views"
)

// Handler builds the application's http.Handler with logging and recovery
// middleware applied to every route.
func Handler(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = views.Index().Render(r.Context(), w)
	})

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static.FS()))))

	return recoverMiddleware(logger)(logMiddleware(logger)(mux))
}
