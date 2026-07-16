// Package hot is the short-term tier: one Redis Stream per device. Streams
// double as the durable buffer (WAL) the archiver drains into cold storage,
// so an entry acknowledged to the producer survives a devlogd crash.
package hot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	devicelogv1 "devlog/api/gen/devicelog/v1"
)

const (
	devicesKey = "devlog:devices"
	chainKey   = "devlog:chain"
	entryField = "e"

	// Group is the archiver's consumer group; unacked entries are reclaimed
	// after a crash, giving at-least-once delivery to cold storage.
	Group = "archiver"
)

func streamKey(device string) string { return "devlog:stream:" + device }

type Store struct {
	rdb       *redis.Client
	retention time.Duration
}

func New(rdb *redis.Client, retention time.Duration) *Store {
	return &Store{rdb: rdb, retention: retention}
}

// Raw is a stream message: the decoded entry plus its stream ID for acking.
type Raw struct {
	ID    string
	Entry *devicelogv1.LogEntry
}

// AppendWithChain stores the signed entry and advances the device's chain
// head in one transaction — they must move together to keep the chain linear.
func (s *Store) AppendWithChain(ctx context.Context, e *devicelogv1.LogEntry) error {
	b, err := proto.Marshal(e)
	if err != nil {
		return err
	}
	// Stream IDs carry the ingest time (ms) so time-range reads and
	// retention trimming operate directly on IDs.
	id := strconv.FormatInt(e.IngestTime.AsTime().UnixMilli(), 10) + "-*"
	pipe := s.rdb.TxPipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{Stream: streamKey(e.DeviceId), ID: id, Values: map[string]any{entryField: b}})
	pipe.HSet(ctx, chainKey, e.DeviceId, string(e.Audit.EntryHash))
	pipe.SAdd(ctx, devicesKey, e.DeviceId)
	_, err = pipe.Exec(ctx)
	return err
}

// ChainHead returns the hash of the device's most recent entry, or nil for a
// device that has never logged.
func (s *Store) ChainHead(ctx context.Context, device string) ([]byte, error) {
	v, err := s.rdb.HGet(ctx, chainKey, device).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(v), nil
}

func (s *Store) Devices(ctx context.Context) ([]string, error) {
	devices, err := s.rdb.SMembers(ctx, devicesKey).Result()
	if err != nil {
		return nil, err
	}
	sort.Strings(devices)
	return devices, nil
}

// Range streams entries with ingest time in [from, to] to fn; fn returns
// false to stop early.
func (s *Store) Range(ctx context.Context, device string, from, to time.Time, fn func(*devicelogv1.LogEntry) bool) error {
	start, end := "-", "+"
	if !from.IsZero() {
		start = strconv.FormatInt(from.UnixMilli(), 10)
	}
	if !to.IsZero() {
		end = strconv.FormatInt(to.UnixMilli(), 10)
	}
	for {
		msgs, err := s.rdb.XRangeN(ctx, streamKey(device), start, end, 1000).Result()
		if err != nil {
			return err
		}
		for _, m := range msgs {
			e, err := decode(m)
			if err != nil {
				return err
			}
			if !fn(e) {
				return nil
			}
		}
		if len(msgs) < 1000 {
			return nil
		}
		start = "(" + msgs[len(msgs)-1].ID // exclusive resume
	}
}

// Trim drops hot entries older than the retention window. Cold storage is
// the system of record beyond this point.
func (s *Store) Trim(ctx context.Context) error {
	devices, err := s.Devices(ctx)
	if err != nil {
		return err
	}
	minID := strconv.FormatInt(time.Now().Add(-s.retention).UnixMilli(), 10)
	for _, d := range devices {
		if err := s.rdb.XTrimMinID(ctx, streamKey(d), minID).Err(); err != nil {
			return fmt.Errorf("trim %s: %w", d, err)
		}
	}
	return nil
}

// RunJanitor trims on a fixed cadence until ctx is cancelled.
func (s *Store) RunJanitor(ctx context.Context, every time.Duration, onErr func(error)) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.Trim(ctx); err != nil && ctx.Err() == nil {
				onErr(err)
			}
		}
	}
}

type DeviceStat struct {
	Device     string
	HotEntries int64
	LastIngest time.Time
}

func (s *Store) Stats(ctx context.Context) ([]DeviceStat, error) {
	devices, err := s.Devices(ctx)
	if err != nil {
		return nil, err
	}
	stats := make([]DeviceStat, 0, len(devices))
	for _, d := range devices {
		n, err := s.rdb.XLen(ctx, streamKey(d)).Result()
		if err != nil {
			return nil, err
		}
		st := DeviceStat{Device: d, HotEntries: n}
		last, err := s.rdb.XRevRangeN(ctx, streamKey(d), "+", "-", 1).Result()
		if err == nil && len(last) == 1 {
			if e, err := decode(last[0]); err == nil {
				st.LastIngest = e.IngestTime.AsTime()
			}
		}
		stats = append(stats, st)
	}
	return stats, nil
}

// --- consumer-group operations used by the archiver ---

func (s *Store) EnsureGroup(ctx context.Context, device string) error {
	err := s.rdb.XGroupCreateMkStream(ctx, streamKey(device), Group, "0").Err()
	if err != nil && strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

// ReadGroup blocks up to `block` for new entries across the given devices.
func (s *Store) ReadGroup(ctx context.Context, devices []string, consumer string, count int64, block time.Duration) (map[string][]Raw, error) {
	streams := make([]string, 0, len(devices)*2)
	for _, d := range devices {
		streams = append(streams, streamKey(d))
	}
	for range devices {
		streams = append(streams, ">")
	}
	res, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    Group,
		Consumer: consumer,
		Streams:  streams,
		Count:    count,
		Block:    block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string][]Raw{}
	for _, stream := range res {
		device := strings.TrimPrefix(stream.Stream, "devlog:stream:")
		for _, m := range stream.Messages {
			e, err := decode(m)
			if err != nil {
				// A corrupt message would wedge the group forever; ack and skip.
				_ = s.rdb.XAck(ctx, stream.Stream, Group, m.ID)
				continue
			}
			out[device] = append(out[device], Raw{ID: m.ID, Entry: e})
		}
	}
	return out, nil
}

func (s *Store) Ack(ctx context.Context, device string, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.rdb.XAck(ctx, streamKey(device), Group, ids...).Err()
}

// ClaimStale takes over entries another (crashed) consumer read but never
// acked, so nothing is lost on the way to cold storage.
func (s *Store) ClaimStale(ctx context.Context, device, consumer string, minIdle time.Duration) ([]Raw, error) {
	var out []Raw
	start := "0-0"
	for {
		msgs, next, err := s.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   streamKey(device),
			Group:    Group,
			Consumer: consumer,
			MinIdle:  minIdle,
			Start:    start,
			Count:    1000,
		}).Result()
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			e, err := decode(m)
			if err != nil {
				_ = s.rdb.XAck(ctx, streamKey(device), Group, m.ID)
				continue
			}
			out = append(out, Raw{ID: m.ID, Entry: e})
		}
		if next == "0-0" || len(msgs) == 0 {
			return out, nil
		}
		start = next
	}
}

func decode(m redis.XMessage) (*devicelogv1.LogEntry, error) {
	v, ok := m.Values[entryField].(string)
	if !ok {
		return nil, fmt.Errorf("stream message %s has no entry field", m.ID)
	}
	e := &devicelogv1.LogEntry{}
	if err := proto.Unmarshal([]byte(v), e); err != nil {
		return nil, fmt.Errorf("decode entry %s: %w", m.ID, err)
	}
	return e, nil
}
