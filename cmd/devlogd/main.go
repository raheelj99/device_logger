// devlogd is the device logging service: embedded MQTT broker (ingestion),
// gRPC LogService (observation), Redis hot tier, bucket cold tier, per-entry
// Ed25519 signatures, and license-gated sessions.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"devlog/internal/archive"
	"devlog/internal/broker"
	"devlog/internal/cold"
	"devlog/internal/config"
	"devlog/internal/grpcapi"
	"devlog/internal/hot"
	"devlog/internal/ingest"
	"devlog/internal/license"
	"devlog/internal/query"
	"devlog/internal/sign"
	"devlog/internal/telemetry"
)

func main() {
	cfgPath := flag.String("config", "config/devlogd.yaml", "path to YAML config")
	flag.Parse()
	if err := run(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "devlogd:", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := telemetry.NewLogger(cfg.Log.Level)
	metrics := telemetry.NewMetrics()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Crypto and licensing.
	signer, err := sign.LoadSigner(cfg.Signing.KeyFile, cfg.Signing.KeyID)
	if err != nil {
		return err
	}
	verifier := sign.NewVerifier()
	verifier.Add(cfg.Signing.KeyID, signer.Public())
	issuerPub, err := sign.LoadPublicPEM(cfg.License.IssuerPubFile)
	if err != nil {
		return fmt.Errorf("load license issuer key: %w", err)
	}
	var online *license.OnlineValidator
	if cfg.License.Mode == "online" {
		if online, err = license.NewOnlineValidator(cfg.License.ServerURL, cfg.License.ServerCAFile,
			cfg.License.Grace.Std(), log); err != nil {
			return err
		}
	}
	sessions := license.NewManager(license.NewVerifier(issuerPub), online, metrics, log)

	// Storage tiers.
	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis unreachable at %s: %w", cfg.Redis.Addr, err)
	}
	hotStore := hot.New(rdb, cfg.Redis.HotRetention.Std())
	objectStore, err := cold.NewMinio(ctx, cfg.S3.Endpoint, cfg.S3.AccessKey, cfg.S3.SecretKey,
		cfg.S3.UseTLS, cfg.S3.Bucket)
	if err != nil {
		return fmt.Errorf("object store unreachable at %s: %w", cfg.S3.Endpoint, err)
	}

	// Pipeline and planes.
	hub := ingest.NewHub()
	pipeline := ingest.NewPipeline(hotStore, signer, hub, metrics, log)
	archiver := archive.New(hotStore, cold.NewWriter(objectStore),
		cfg.Archive.FlushInterval.Std(), cfg.Archive.MaxBatchBytes, cfg.Archive.MaxBatchEntries, metrics, log)
	engine := query.New(hotStore, cold.NewReader(objectStore), signer, verifier,
		cfg.Query.MaxResults, cfg.Query.DefaultLookback.Std())

	mqttTLS, err := config.ServerTLS(cfg.MQTT.TLS)
	if err != nil {
		return fmt.Errorf("mqtt tls: %w", err)
	}
	grpcTLS, err := config.ServerTLS(cfg.GRPC.TLS)
	if err != nil {
		return fmt.Errorf("grpc tls: %w", err)
	}
	brk, err := broker.New(cfg.MQTT.Listen, mqttTLS, pipeline, sessions, metrics, log)
	if err != nil {
		return err
	}
	grpcServer := grpcapi.NewServer(grpcTLS, sessions,
		grpcapi.NewService(engine, hub, hotStore, log), metrics, log)

	var ready atomic.Bool
	httpSrv := telemetry.NewHTTP(cfg.HTTP.Listen, metrics.Registry, ready.Load, log)

	// Run every long-lived component under one errgroup: the first failure
	// (or a signal) cancels ctx and unwinds the rest gracefully.
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return brk.Run(ctx) })
	g.Go(func() error { return archiver.Run(ctx) })
	g.Go(func() error {
		return hotStore.RunJanitor(ctx, 5*time.Minute, func(err error) {
			log.Error("hot retention trim failed", "err", err)
		})
	})
	g.Go(func() error { return httpSrv.Run(ctx) })

	lis, err := net.Listen("tcp", cfg.GRPC.Listen)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	g.Go(func() error { return grpcServer.Serve(lis) })
	g.Go(func() error {
		<-ctx.Done()
		grpcServer.GracefulStop()
		return nil
	})

	ready.Store(true)
	log.Info("devlogd up",
		"mqtt", cfg.MQTT.Listen, "grpc", cfg.GRPC.Listen, "http", cfg.HTTP.Listen,
		"license_mode", cfg.License.Mode, "signing_key", cfg.Signing.KeyID)
	err = g.Wait()
	log.Info("devlogd stopped")
	return err
}
