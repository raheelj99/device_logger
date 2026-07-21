// Package bootstrap optionally spawns a backing service (Redis, MinIO) as a
// local child process when nothing answers at its configured address yet,
// so a single devlogd deployment can be self-contained on a station that
// isn't handed an already-running Redis/MinIO. It never touches an instance
// it did not itself start, and it only acts once at startup — it is not a
// runtime supervisor that watches for or restarts a process that dies later.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"devlog/internal/config"
)

// Service is a handle to a process this package spawned. A nil *Service
// (returned alongside a nil error) means nothing was spawned — either the
// address was already reachable, or auto-start is disabled — and there is
// nothing to stop later.
type Service struct {
	name string
	cmd  *exec.Cmd
	exit chan error
	log  *slog.Logger
}

// dialOK reports whether addr is currently accepting TCP connections.
func dialOK(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ensure is the shared lifecycle for both Redis and MinIO: if addr is
// already reachable, or auto-start is disabled, it returns (nil, nil) and
// leaves the existing connect-and-verify step in the caller to produce the
// real error. Otherwise it resolves the binary, creates the data directory,
// starts buildCmd, and blocks until addr accepts connections, the process
// exits early, ctx is cancelled, or the timeout elapses.
func ensure(ctx context.Context, name, addr string, auto config.AutoStart, log *slog.Logger,
	buildCmd func(dataDir, host, port string) *exec.Cmd) (*Service, error) {
	if dialOK(addr) {
		return nil, nil
	}
	if !auto.Enabled {
		return nil, nil
	}

	binPath, err := exec.LookPath(auto.BinPath)
	if err != nil {
		return nil, fmt.Errorf("%s auto_start enabled but %q not found on PATH: %w (install it, or set auto_start.bin_path)", name, auto.BinPath, err)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid address %q: %w", name, addr, err)
	}
	dataDir := auto.DataDir
	if dataDir == "" {
		dataDir = filepath.Join("data", name)
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("%s: create data dir %s: %w", name, dataDir, err)
	}
	logFile, err := os.OpenFile(filepath.Join(dataDir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("%s: open log file: %w", name, err)
	}

	cmd := buildCmd(dataDir, host, port)
	cmd.Path = binPath
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("%s: start %s: %w", name, binPath, err)
	}
	log.Info("spawned "+name, "bin", binPath, "addr", addr, "data_dir", dataDir, "pid", cmd.Process.Pid)

	svc := &Service{name: name, cmd: cmd, exit: make(chan error, 1), log: log}
	go func() { svc.exit <- cmd.Wait(); logFile.Close() }()

	timeout := auto.Timeout.Std()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	for {
		if dialOK(addr) {
			return svc, nil
		}
		select {
		case err := <-svc.exit:
			return nil, fmt.Errorf("%s exited before becoming ready (see %s): %w", name, logFile.Name(), err)
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, ctx.Err()
		case <-deadline.C:
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("%s did not become ready within %s (see %s)", name, timeout, logFile.Name())
		case <-poll.C:
		}
	}
}

// Stop gracefully drains a process this package spawned: SIGTERM, then a
// bounded wait, then SIGKILL. It is a no-op for a nil Service (the common
// case — nothing was spawned because the address was already reachable or
// auto-start was disabled), so callers can unconditionally defer it.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.log.Info("stopping "+s.name, "pid", s.cmd.Process.Pid)
	// SIGTERM is a best-effort graceful stop; Windows doesn't support
	// delivering it to another process, so this falls straight through to
	// the Kill() below there.
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = s.cmd.Process.Kill()
	}
	select {
	case <-s.exit:
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.exit
	}
}
