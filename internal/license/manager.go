package license

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"devlog/internal/telemetry"
)

// Manager is the single authorization entry point used by both the MQTT auth
// hook and the gRPC interceptors.
type Manager struct {
	offline *Verifier
	online  *OnlineValidator // nil in offline mode
	metrics *telemetry.Metrics
	log     *slog.Logger

	mu     sync.Mutex
	active map[string]struct{} // connection/session ids, for the gauge
}

func NewManager(offline *Verifier, online *OnlineValidator, metrics *telemetry.Metrics, log *slog.Logger) *Manager {
	return &Manager{
		offline: offline,
		online:  online,
		metrics: metrics,
		log:     log,
		active:  map[string]struct{}{},
	}
}

// Authorize verifies a presented license token. Offline verification is
// always performed; online activation is added when configured.
func (m *Manager) Authorize(ctx context.Context, token, subject, feature string) (*Signed, error) {
	s, err := Parse(token)
	if err != nil {
		return nil, err
	}
	if err := m.offline.Verify(s, subject, feature, time.Now()); err != nil {
		return nil, err
	}
	if m.online != nil {
		if err := m.online.Validate(ctx, s, subject); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (m *Manager) SessionStarted(id string) {
	m.mu.Lock()
	m.active[id] = struct{}{}
	m.metrics.LicenseSessions.Set(float64(len(m.active)))
	m.mu.Unlock()
}

func (m *Manager) SessionEnded(id string) {
	m.mu.Lock()
	delete(m.active, id)
	m.metrics.LicenseSessions.Set(float64(len(m.active)))
	m.mu.Unlock()
}
