// Package archive drains the hot tier into cold segments through a Redis
// consumer group: entries are acked only after the segment is durably in the
// bucket, so delivery to cold storage is at-least-once (queries deduplicate
// by entry ID).
package archive

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	devicelogv1 "devlog/api/gen/devicelog/v1"
	"devlog/internal/cold"
	"devlog/internal/hot"
	"devlog/internal/telemetry"
)

type Archiver struct {
	hot      *hot.Store
	writer   *cold.Writer
	interval time.Duration
	maxBytes int
	maxCount int
	metrics  *telemetry.Metrics
	log      *slog.Logger
	consumer string

	batch      []*devicelogv1.LogEntry
	batchBytes int
	acks       map[string][]string // device → stream ids awaiting ack
	lastFlush  time.Time
}

func New(h *hot.Store, w *cold.Writer, interval time.Duration, maxBytes, maxCount int,
	metrics *telemetry.Metrics, log *slog.Logger) *Archiver {
	return &Archiver{
		hot: h, writer: w,
		interval: interval, maxBytes: maxBytes, maxCount: maxCount,
		metrics: metrics, log: log,
		consumer: "archiver-1",
		acks:     map[string][]string{},
	}
}

func (a *Archiver) Run(ctx context.Context) error {
	a.reclaim(ctx)
	a.lastFlush = time.Now()
	for {
		if ctx.Err() != nil {
			a.finalFlush()
			return nil
		}
		devices, err := a.hot.Devices(ctx)
		if err != nil {
			a.pause(ctx, err)
			continue
		}
		if len(devices) == 0 {
			a.sleep(ctx, time.Second)
			continue
		}
		groupsOK := true
		for _, d := range devices {
			if err := a.hot.EnsureGroup(ctx, d); err != nil {
				a.pause(ctx, err)
				groupsOK = false
				break
			}
		}
		if !groupsOK {
			continue
		}
		// Batch already full (previous flush failed): retry the flush with
		// backoff instead of reading more — keeps memory bounded during a
		// bucket outage while the backlog waits safely in Redis.
		if len(a.batch) >= a.maxCount || a.batchBytes >= a.maxBytes {
			a.flush(ctx)
			if len(a.batch) > 0 {
				a.sleep(ctx, time.Second)
			}
			continue
		}
		msgs, err := a.hot.ReadGroup(ctx, devices, a.consumer, int64(a.maxCount-len(a.batch)), 2*time.Second)
		if err != nil && ctx.Err() == nil {
			a.pause(ctx, err)
			continue
		}
		for device, raws := range msgs {
			for _, raw := range raws {
				a.batch = append(a.batch, raw.Entry)
				a.batchBytes += proto.Size(raw.Entry)
				a.acks[device] = append(a.acks[device], raw.ID)
			}
		}
		full := len(a.batch) >= a.maxCount || a.batchBytes >= a.maxBytes
		due := time.Since(a.lastFlush) >= a.interval && len(a.batch) > 0
		if full || due {
			a.flush(ctx)
		}
	}
}

// flush writes the batch as one segment; on failure the batch is retained
// and retried, and unacked entries survive a crash via the consumer group.
func (a *Archiver) flush(ctx context.Context) {
	if len(a.batch) == 0 {
		return
	}
	start := time.Now()
	m, err := a.writer.Write(ctx, a.batch)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Error("segment flush failed, batch retained", "entries", len(a.batch), "err", err)
		}
		return
	}
	for device, ids := range a.acks {
		if err := a.hot.Ack(ctx, device, ids...); err != nil && ctx.Err() == nil {
			// Segment is durable; a failed ack only risks re-archiving,
			// which query-side dedupe absorbs.
			a.log.Warn("ack failed after flush", "device", device, "err", err)
		}
	}
	a.metrics.SegmentFlushSeconds.Observe(time.Since(start).Seconds())
	a.metrics.SegmentsFlushed.Inc()
	a.metrics.SegmentBytes.Add(float64(m.Bytes))
	a.log.Info("segment flushed", "key", m.SegmentKey, "entries", m.Count, "devices", len(m.Devices))
	a.batch = nil
	a.batchBytes = 0
	a.acks = map[string][]string{}
	a.lastFlush = time.Now()
}

// finalFlush runs during shutdown with a fresh context so draining is not
// aborted by the cancellation that triggered it.
func (a *Archiver) finalFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a.flush(ctx)
}

// reclaim adopts entries a previous (crashed) run read but never acked.
func (a *Archiver) reclaim(ctx context.Context) {
	devices, err := a.hot.Devices(ctx)
	if err != nil {
		return
	}
	for _, d := range devices {
		if err := a.hot.EnsureGroup(ctx, d); err != nil {
			continue
		}
		raws, err := a.hot.ClaimStale(ctx, d, a.consumer, time.Minute)
		if err != nil || len(raws) == 0 {
			continue
		}
		a.log.Info("reclaimed unarchived entries", "device", d, "entries", len(raws))
		for _, raw := range raws {
			a.batch = append(a.batch, raw.Entry)
			a.batchBytes += proto.Size(raw.Entry)
			a.acks[d] = append(a.acks[d], raw.ID)
		}
	}
}

func (a *Archiver) pause(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	a.log.Error("archiver error, backing off", "err", err)
	a.sleep(ctx, time.Second)
}

func (a *Archiver) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
