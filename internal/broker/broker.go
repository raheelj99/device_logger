// Package broker embeds the MQTT broker (mochi-mqtt): TLS termination,
// license-based authentication, topic ACLs, and handoff of published
// payloads into the ingest pipeline — all inside the devlogd process.
package broker

import (
	"context"
	"crypto/tls"
	"log/slog"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"

	"devlog/internal/ingest"
	"devlog/internal/license"
	"devlog/internal/telemetry"
)

type Broker struct {
	srv *mqtt.Server
	log *slog.Logger
}

func New(listen string, tlsCfg *tls.Config, pipeline *ingest.Pipeline,
	sessions *license.Manager, metrics *telemetry.Metrics, log *slog.Logger) (*Broker, error) {
	srv := mqtt.New(&mqtt.Options{InlineClient: false, Logger: log.With("component", "mqtt")})
	if err := srv.AddHook(&authHook{sessions: sessions, metrics: metrics, log: log}, nil); err != nil {
		return nil, err
	}
	if err := srv.AddHook(&ingestHook{pipeline: pipeline, log: log}, nil); err != nil {
		return nil, err
	}
	if err := srv.AddListener(listeners.NewTCP(listeners.Config{
		ID:        "mqtts",
		Address:   listen,
		TLSConfig: tlsCfg,
	})); err != nil {
		return nil, err
	}
	return &Broker{srv: srv, log: log}, nil
}

// Run serves until ctx is cancelled.
func (b *Broker) Run(ctx context.Context) error {
	if err := b.srv.Serve(); err != nil {
		return err
	}
	<-ctx.Done()
	return b.srv.Close()
}
