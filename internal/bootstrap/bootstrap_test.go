package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"devlog/internal/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// freeAddr reserves an ephemeral port on loopback and immediately releases
// it, giving a real, currently-unreachable address to auto-start against.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestEnsureSkipsWhenAlreadyReachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	svc, err := ensure(context.Background(), "test", l.Addr().String(),
		config.AutoStart{Enabled: true, BinPath: "does-not-matter"}, testLogger(),
		func(dataDir, host, port string) *exec.Cmd {
			t.Fatal("buildCmd must not be called when the address is already reachable")
			return nil
		})
	if err != nil || svc != nil {
		t.Fatalf("ensure() = (%v, %v), want (nil, nil)", svc, err)
	}
}

func TestEnsureSkipsWhenDisabled(t *testing.T) {
	svc, err := ensure(context.Background(), "test", freeAddr(t),
		config.AutoStart{Enabled: false}, testLogger(),
		func(dataDir, host, port string) *exec.Cmd {
			t.Fatal("buildCmd must not be called when auto-start is disabled")
			return nil
		})
	if err != nil || svc != nil {
		t.Fatalf("ensure() = (%v, %v), want (nil, nil) so the caller's own fail-fast connect still runs", svc, err)
	}
}

func TestEnsureSpawnsWaitsAndStops(t *testing.T) {
	addr := freeAddr(t)
	auto := config.AutoStart{
		Enabled: true, BinPath: os.Args[0], DataDir: t.TempDir(),
		Timeout: config.Duration(5 * time.Second),
	}
	svc, err := ensure(context.Background(), "test", addr, auto, testLogger(),
		func(dataDir, host, port string) *exec.Cmd {
			cmd := exec.Command(os.Args[0], "-test.run=TestHelperListener")
			cmd.Env = append(os.Environ(),
				"GO_WANT_HELPER_PROCESS=1",
				"HELPER_LISTEN_ADDR="+net.JoinHostPort(host, port))
			return cmd
		})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if svc == nil {
		t.Fatal("expected a non-nil Service for a process this call spawned")
	}
	if !dialOK(addr) {
		t.Fatal("expected addr to be reachable once ensure() returns")
	}

	svc.Stop()
	for i := 0; i < 20 && dialOK(addr); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if dialOK(addr) {
		t.Fatal("expected the spawned process to stop listening after Stop()")
	}
}

func TestEnsureReportsProcessThatExitsBeforeReady(t *testing.T) {
	auto := config.AutoStart{
		Enabled: true, BinPath: os.Args[0], DataDir: t.TempDir(),
		Timeout: config.Duration(2 * time.Second),
	}
	_, err := ensure(context.Background(), "test", freeAddr(t), auto, testLogger(),
		func(dataDir, host, port string) *exec.Cmd {
			// "-test.run=^$" matches no tests: the subprocess exits almost
			// immediately without ever listening on the address.
			return exec.Command(os.Args[0], "-test.run=^$")
		})
	if err == nil {
		t.Fatal("expected an error when the spawned process exits before the address becomes reachable")
	}
}

// TestHelperListener is not a real test. It is invoked as a subprocess by
// TestEnsureSpawnsWaitsAndStops to stand in for a real backing service:
// listen on the requested address until killed.
func TestHelperListener(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	l, err := net.Listen("tcp", os.Getenv("HELPER_LISTEN_ADDR"))
	if err != nil {
		os.Exit(1)
	}
	defer l.Close()
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}
