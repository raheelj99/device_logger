package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	devicelogv1 "devlog/api/gen/devicelog/v1"
	"devlog/internal/hot"
	"devlog/internal/sign"
	"devlog/internal/telemetry"
)

type Pipeline struct {
	hot     *hot.Store
	signer  *sign.Signer
	hub     *Hub
	metrics *telemetry.Metrics
	log     *slog.Logger

	// mu serializes hash-chain updates: each entry must link to the true
	// previous entry, so chain-read → sign → append is one critical section.
	mu     sync.Mutex
	chains map[string][]byte
}

func NewPipeline(h *hot.Store, s *sign.Signer, hub *Hub, m *telemetry.Metrics, log *slog.Logger) *Pipeline {
	return &Pipeline{hot: h, signer: s, hub: hub, metrics: m, log: log, chains: map[string][]byte{}}
}

// Ingest processes one raw MQTT payload from an authenticated device.
func (p *Pipeline) Ingest(ctx context.Context, deviceID, subsystem string, payload []byte) error {
	e := &devicelogv1.LogEntry{}
	if err := proto.Unmarshal(payload, e); err != nil {
		p.metrics.IngestErrors.WithLabelValues("decode").Inc()
		return fmt.Errorf("payload is not a LogEntry: %w", err)
	}
	// The authenticated MQTT identity is authoritative — a device cannot
	// write entries under another device's name.
	if e.DeviceId == "" {
		e.DeviceId = deviceID
	} else if e.DeviceId != deviceID {
		p.metrics.IngestErrors.WithLabelValues("identity_mismatch").Inc()
		return fmt.Errorf("entry claims device %q but session is %q", e.DeviceId, deviceID)
	}
	if e.Subsystem == "" {
		e.Subsystem = subsystem
	}
	e.EntryId = ulid.Make().String()
	e.IngestTime = timestamppb.Now()
	if e.DeviceTime == nil {
		e.DeviceTime = e.IngestTime
	}
	e.Audit = nil // server-owned; ignore whatever the producer sent

	start := time.Now()
	p.mu.Lock()
	prev, ok := p.chains[e.DeviceId]
	if !ok {
		var err error
		if prev, err = p.hot.ChainHead(ctx, e.DeviceId); err != nil {
			p.mu.Unlock()
			p.metrics.IngestErrors.WithLabelValues("store").Inc()
			return err
		}
	}
	if err := sign.ChainSign(e, prev, p.signer); err != nil {
		p.mu.Unlock()
		p.metrics.IngestErrors.WithLabelValues("sign").Inc()
		return err
	}
	if err := p.hot.AppendWithChain(ctx, e); err != nil {
		// Not advancing the in-memory chain keeps the next entry linked to
		// the last durably stored one.
		delete(p.chains, e.DeviceId)
		p.mu.Unlock()
		p.metrics.IngestErrors.WithLabelValues("store").Inc()
		return err
	}
	p.chains[e.DeviceId] = e.Audit.EntryHash
	p.mu.Unlock()

	p.hub.Publish(e)
	p.observe(e, len(payload), time.Since(start))
	return nil
}

func (p *Pipeline) observe(e *devicelogv1.LogEntry, payloadBytes int, took time.Duration) {
	p.metrics.HotAppendSeconds.Observe(took.Seconds())
	p.metrics.EntriesIngested.WithLabelValues(e.DeviceId, severityLabel(e.Severity)).Inc()
	p.metrics.IngestBytes.Add(float64(payloadBytes))
	if s := e.Sanitization; s != nil {
		p.metrics.SanitizationPhase.WithLabelValues(phaseLabel(s.Phase)).Inc()
		if v := s.Verification; v != nil {
			result := "failed"
			if v.Passed {
				result = "passed"
			}
			p.metrics.Verification.WithLabelValues(result).Inc()
		}
	}
}

func severityLabel(s devicelogv1.Severity) string {
	switch s {
	case devicelogv1.Severity_SEVERITY_TRACE:
		return "trace"
	case devicelogv1.Severity_SEVERITY_DEBUG:
		return "debug"
	case devicelogv1.Severity_SEVERITY_INFO:
		return "info"
	case devicelogv1.Severity_SEVERITY_WARN:
		return "warn"
	case devicelogv1.Severity_SEVERITY_ERROR:
		return "error"
	case devicelogv1.Severity_SEVERITY_FATAL:
		return "fatal"
	default:
		return "unspecified"
	}
}

func phaseLabel(p devicelogv1.SanitizationPhase) string {
	switch p {
	case devicelogv1.SanitizationPhase_SANITIZATION_PHASE_STARTED:
		return "started"
	case devicelogv1.SanitizationPhase_SANITIZATION_PHASE_PROGRESS:
		return "progress"
	case devicelogv1.SanitizationPhase_SANITIZATION_PHASE_VERIFYING:
		return "verifying"
	case devicelogv1.SanitizationPhase_SANITIZATION_PHASE_COMPLETED:
		return "completed"
	case devicelogv1.SanitizationPhase_SANITIZATION_PHASE_FAILED:
		return "failed"
	case devicelogv1.SanitizationPhase_SANITIZATION_PHASE_ABORTED:
		return "aborted"
	default:
		return "unspecified"
	}
}
