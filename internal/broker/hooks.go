package broker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"devlog/internal/ingest"
	"devlog/internal/license"
	"devlog/internal/telemetry"
)

const topicPrefix = "devlog/v1/"

// authHook enforces license-as-credential at CONNECT time and pins each
// session to its own topic namespace.
type authHook struct {
	mqtt.HookBase
	sessions *license.Manager
	metrics  *telemetry.Metrics
	log      *slog.Logger
}

func (h *authHook) ID() string { return "license-auth" }

func (h *authHook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnConnectAuthenticate,
		mqtt.OnACLCheck,
		mqtt.OnSessionEstablished,
		mqtt.OnDisconnect,
	}, []byte{b})
}

// OnConnectAuthenticate: MQTT username is the device ID, password is the
// signed license token.
func (h *authHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	device := string(pk.Connect.Username)
	token := string(pk.Connect.Password)
	if device == "" || token == "" {
		h.log.Warn("mqtt connect without credentials", "remote", cl.Net.Remote)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.sessions.Authorize(ctx, token, device, license.FeatureIngest); err != nil {
		h.log.Warn("mqtt session rejected", "device", device, "err", err)
		return false
	}
	h.log.Info("mqtt session authorized", "device", device)
	return true
}

// OnACLCheck: producers may only publish, and only under their own device
// prefix — one compromised device cannot write as another.
func (h *authHook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	if !write {
		return false
	}
	return strings.HasPrefix(topic, topicPrefix+string(cl.Properties.Username)+"/")
}

func (h *authHook) OnSessionEstablished(cl *mqtt.Client, _ packets.Packet) {
	h.metrics.MQTTConnections.Inc()
	h.sessions.SessionStarted(cl.ID)
}

func (h *authHook) OnDisconnect(cl *mqtt.Client, _ error, _ bool) {
	h.metrics.MQTTConnections.Dec()
	h.sessions.SessionEnded(cl.ID)
}

// ingestHook feeds every accepted publish into the pipeline.
type ingestHook struct {
	mqtt.HookBase
	pipeline *ingest.Pipeline
	log      *slog.Logger
}

func (h *ingestHook) ID() string { return "ingest" }

func (h *ingestHook) Provides(b byte) bool {
	return b == mqtt.OnPublish
}

func (h *ingestHook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	device := string(cl.Properties.Username)
	subsystem := ""
	// Topic layout: devlog/v1/{device_id}/{subsystem}
	if parts := strings.SplitN(pk.TopicName, "/", 4); len(parts) == 4 {
		subsystem = parts[3]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.pipeline.Ingest(ctx, device, subsystem, pk.Payload); err != nil {
		// Reject the malformed/failed publish without killing the session.
		h.log.Warn("ingest rejected", "device", device, "topic", pk.TopicName, "err", err)
	}
	return pk, nil
}
