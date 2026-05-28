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
RUN CGO_ENABLED=0 go build -ldflags='-s -w' -trimpath -o /out/mere-server ./cmd/server

# Runtime: minimal alpine + non-root user. Migrations are embedded in the
# binary via the //go:embed directive in migrations/migrations.go, so no
# need to COPY /migrations into this stage.
FROM alpine:3.19
RUN adduser -D -u 65532 mere
COPY --from=build /out/mere-server /mere-server
USER mere
ENTRYPOINT ["/mere-server"]
