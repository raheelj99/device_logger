// licensed is the mini license server: it activates and heartbeats sessions
// for devlogd instances running in online licensing mode.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"devlog/internal/license"
	"devlog/internal/sign"
	"devlog/internal/telemetry"
)

func main() {
	listen := flag.String("listen", ":9444", "listen address")
	issuerPub := flag.String("issuer-pub", "deploy/keys/issuer.pub", "issuer public key (PEM)")
	certFile := flag.String("cert", "deploy/certs/server.crt", "TLS certificate")
	keyFile := flag.String("key", "deploy/certs/server.key", "TLS private key")
	level := flag.String("log-level", "info", "log level")
	flag.Parse()

	if err := run(*listen, *issuerPub, *certFile, *keyFile, *level); err != nil {
		fmt.Fprintln(os.Stderr, "licensed:", err)
		os.Exit(1)
	}
}

func run(listen, issuerPub, certFile, keyFile, level string) error {
	log := telemetry.NewLogger(level)
	pub, err := sign.LoadPublicPEM(issuerPub)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           license.NewServer(pub, log).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServeTLS(certFile, keyFile) }()
	log.Info("licensed up", "listen", listen)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
