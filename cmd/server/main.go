// Command mere-server is the analytics application binary. The whole boot
// sequence and SIGTERM choreography live in the importable app package
// (ADR-0003); main is a thin shim that constructs the logger and forwards its
// build-time Version stamp.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jjdinho/mere-analytics/app"
)

// Version is the build-time version stamp, injected via
// -ldflags="-X main.Version=$(git describe --tags --always --dirty)" by the
// Dockerfile / CI. It defaults to "dev" for plain `go build` / `go run`. The
// value is logged on boot and returned in the /healthz JSON body so operators
// can answer "what's deployed right now?" from `kamal logs` or a probe.
var Version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if err := app.Run(context.Background(), app.WithLogger(logger), app.WithVersion(Version)); err != nil {
		logger.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}
