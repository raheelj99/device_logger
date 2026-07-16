# Multi-stage build: compile static binaries, ship them on distroless.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# Distroless: no shell, no package manager — minimal attack surface.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/devlogd /out/licensed /out/lictl /out/logctl /
ENTRYPOINT ["/devlogd"]
