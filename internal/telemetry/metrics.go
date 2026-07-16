package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics is the single registry of everything devlogd exports to Prometheus.
// Passing this struct around (instead of package-level globals) keeps metric
// ownership explicit and tests isolated.
type Metrics struct {
	Registry *prometheus.Registry

	EntriesIngested     *prometheus.CounterVec // device, severity
	IngestBytes         prometheus.Counter
	IngestErrors        *prometheus.CounterVec // reason
	SanitizationPhase   *prometheus.CounterVec // phase
	Verification        *prometheus.CounterVec // result
	HotAppendSeconds    prometheus.Histogram
	SegmentFlushSeconds prometheus.Histogram
	SegmentsFlushed     prometheus.Counter
	SegmentBytes        prometheus.Counter
	MQTTConnections     prometheus.Gauge
	LicenseSessions     prometheus.Gauge
	GRPCRequests        *prometheus.CounterVec // method, code
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	f := promauto.With(reg)
	return &Metrics{
		Registry: reg,
		EntriesIngested: f.NewCounterVec(prometheus.CounterOpts{
			Name: "devlog_entries_ingested_total", Help: "Log entries accepted, by device and severity.",
		}, []string{"device", "severity"}),
		IngestBytes: f.NewCounter(prometheus.CounterOpts{
			Name: "devlog_ingest_bytes_total", Help: "Raw payload bytes accepted over MQTT.",
		}),
		IngestErrors: f.NewCounterVec(prometheus.CounterOpts{
			Name: "devlog_ingest_errors_total", Help: "Rejected or failed ingests, by reason.",
		}, []string{"reason"}),
		SanitizationPhase: f.NewCounterVec(prometheus.CounterOpts{
			Name: "devlog_sanitization_phase_total", Help: "Sanitization events, by phase.",
		}, []string{"phase"}),
		Verification: f.NewCounterVec(prometheus.CounterOpts{
			Name: "devlog_verification_total", Help: "Sanitization verification outcomes.",
		}, []string{"result"}),
		HotAppendSeconds: f.NewHistogram(prometheus.HistogramOpts{
			Name: "devlog_hot_append_seconds", Help: "Latency of Redis appends.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 12),
		}),
		SegmentFlushSeconds: f.NewHistogram(prometheus.HistogramOpts{
			Name: "devlog_segment_flush_seconds", Help: "Latency of cold-storage segment flushes.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
		}),
		SegmentsFlushed: f.NewCounter(prometheus.CounterOpts{
			Name: "devlog_segments_flushed_total", Help: "Segments written to the bucket.",
		}),
		SegmentBytes: f.NewCounter(prometheus.CounterOpts{
			Name: "devlog_segment_bytes_total", Help: "Compressed bytes written to the bucket.",
		}),
		MQTTConnections: f.NewGauge(prometheus.GaugeOpts{
			Name: "devlog_mqtt_connections", Help: "Currently connected MQTT clients.",
		}),
		LicenseSessions: f.NewGauge(prometheus.GaugeOpts{
			Name: "devlog_license_sessions", Help: "Currently active licensed sessions.",
		}),
		GRPCRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "devlog_grpc_requests_total", Help: "gRPC requests, by method and status code.",
		}, []string{"method", "code"}),
	}
}
