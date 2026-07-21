package bootstrap

import (
	"context"
	"log/slog"
	"os/exec"

	"devlog/internal/config"
)

// EnsureRedis spawns a local redis-server bound to addr, with AOF enabled so
// the hot tier's durability guarantee holds, if addr isn't already reachable
// and auto.Enabled is set. See [ensure] for the shared lifecycle.
func EnsureRedis(ctx context.Context, addr, password string, auto config.AutoStart, log *slog.Logger) (*Service, error) {
	return ensure(ctx, "redis", addr, auto, log, func(dataDir, host, port string) *exec.Cmd {
		args := []string{"--port", port, "--dir", dataDir, "--appendonly", "yes"}
		if host != "" {
			args = append(args, "--bind", host)
		}
		if password != "" {
			args = append(args, "--requirepass", password)
		}
		return exec.Command(auto.BinPath, args...)
	})
}
