# syntax=docker/dockerfile:1.6

# Builder: Go 1.25 (pinned to match go.mod's minimum). Templ CLI is also pinned
# (must match the github.com/a-h/templ runtime version in go.mod).
FROM golang:1.25-alpine AS build
WORKDIR /src

RUN go install github.com/a-h/templ/cmd/templ@v0.3.1020

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN templ generate

# Version stamp injected into the server binary. Kamal/CI pass
# --build-arg VERSION=$(git describe --tags --always --dirty); a plain
# `docker build` falls back to "dev". main.Version is logged on boot and
# returned in the /healthz body.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=${VERSION}" -trimpath -o /out/mere-server ./cmd/server
RUN CGO_ENABLED=0 go build -ldflags='-s -w' -trimpath -o /out/mere-maintenance ./cmd/maintenance

# Runtime: minimal alpine + non-root user. Migrations are embedded in the
# binary via the //go:embed directive in migrations/migrations.go, so no
# need to COPY /migrations into this stage.
#
# Two binaries ship in the image: mere-server (the HTTP entrypoint) and
# mere-maintenance (one-shot cleanup, invoked by host cron / Kamal scheduled
# task; never started in-process by the server).
FROM alpine:3.19
RUN adduser -D -u 65532 mere
COPY --from=build /out/mere-server /mere-server
COPY --from=build /out/mere-maintenance /mere-maintenance
USER mere
ENTRYPOINT ["/mere-server"]
