// Package telemetry owns the service's own observability: structured logs,
// Prometheus metrics, and the health/metrics HTTP endpoint.
package telemetry

import (
	"log/slog"
	"os"
)

// NewLogger returns a JSON slog logger; JSON keeps the service's own logs
// machine-parseable for Loki/ELK pipelines.
func NewLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
