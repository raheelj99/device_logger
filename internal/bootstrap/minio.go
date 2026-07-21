package bootstrap

import (
	"context"
	"log/slog"
	"os"
	"os/exec"

	"devlog/internal/config"
)

// EnsureMinio spawns a local minio server bound to endpoint, credentialed
// with accessKey/secretKey (the same credentials devlogd itself connects
// with), if endpoint isn't already reachable and auto.Enabled is set. See
// [ensure] for the shared lifecycle.
func EnsureMinio(ctx context.Context, endpoint, accessKey, secretKey string, auto config.AutoStart, log *slog.Logger) (*Service, error) {
	return ensure(ctx, "minio", endpoint, auto, log, func(dataDir, host, port string) *exec.Cmd {
		cmd := exec.Command(auto.BinPath, "server", dataDir, "--address", host+":"+port)
		cmd.Env = append(os.Environ(),
			"MINIO_ROOT_USER="+accessKey,
			"MINIO_ROOT_PASSWORD="+secretKey,
		)
		return cmd
	})
}
