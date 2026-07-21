package archive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/timestamppb"

	devicelogv1 "devlog/api/gen/devicelog/v1"
	"devlog/internal/cold"
	"devlog/internal/hot"
	"devlog/internal/telemetry"
)

// fakeStore is an in-memory ObjectStore for tests (mirrors internal/cold's).
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{objects: map[string][]byte{}} }

func (f *fakeStore) Put(_ context.Context, key string, data []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), data...)
	return nil
}

func (f *fakeStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("no such object %s", key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeStore) List(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func newTestArchiver(t *testing.T, maxBatchEntries int) (*Archiver, *hot.Store, *fakeStore) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	hotStore := hot.New(rdb, 24*time.Hour)
	store := newFakeStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(hotStore, cold.NewWriter(store), time.Hour, 8<<20, maxBatchEntries, telemetry.NewMetrics(), log)
	return a, hotStore, store
}

func testEntry(device string, at time.Time, id string) *devicelogv1.LogEntry {
	return &devicelogv1.LogEntry{
		EntryId:    id,
		DeviceId:   device,
		IngestTime: timestamppb.New(at),
		Message:    id,
		Audit:      &devicelogv1.Audit{EntryHash: []byte(id)},
	}
}

// allEntries reads back every entry archived across every segment currently
// in store, via the same manifest-pruning path the query engine uses.
func allEntries(t *testing.T, store *fakeStore, from, to time.Time) []*devicelogv1.LogEntry {
	t.Helper()
	ctx := context.Background()
	ms, err := cold.NewReader(store).Manifests(ctx, from, to, nil)
	if err != nil {
		t.Fatalf("manifests: %v", err)
	}
	var out []*devicelogv1.LogEntry
	for _, m := range ms {
		if err := cold.NewReader(store).Read(ctx, m, func(e *devicelogv1.LogEntry) bool {
			out = append(out, e)
			return true
		}); err != nil {
			t.Fatalf("read segment: %v", err)
		}
	}
	return out
}

func TestFinalFlushBacksUpEntriesNeverReadByMainLoop(t *testing.T) {
	a, hotStore, store := newTestArchiver(t, 100)
	ctx := context.Background()
	now := time.Now()

	// Entries land in the hot tier the way ingest normally would — the
	// archiver's read loop never ran, so nothing is in a.batch yet. This is
	// exactly devlogd's shutdown scenario: entries sitting in Redis that the
	// main loop hasn't gotten around to reading.
	want := []string{"a", "b", "c"}
	for i, id := range want {
		if err := hotStore.AppendWithChain(ctx, testEntry("dev1", now.Add(time.Duration(i)*time.Second), id)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	a.finalFlush()

	got := allEntries(t, store, now.Add(-time.Minute), now.Add(time.Minute))
	if len(got) != len(want) {
		t.Fatalf("expected all %d pending entries backed up to cold storage, got %d: %+v", len(want), len(got), got)
	}
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.EntryId] = true
	}
	for _, id := range want {
		if !ids[id] {
			t.Errorf("entry %q missing from cold storage after finalFlush", id)
		}
	}
}

func TestFinalFlushSplitsLargeBacklogAcrossSegments(t *testing.T) {
	// A small per-batch entry cap forces drainAll to flush more than once
	// instead of holding an unbounded batch in memory during shutdown.
	a, hotStore, store := newTestArchiver(t, 2)
	ctx := context.Background()
	now := time.Now()

	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("e%d", i)
		if err := hotStore.AppendWithChain(ctx, testEntry("dev1", now.Add(time.Duration(i)*time.Second), id)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	a.finalFlush()

	got := allEntries(t, store, now.Add(-time.Minute), now.Add(time.Minute))
	if len(got) != 5 {
		t.Fatalf("expected all 5 entries backed up, got %d", len(got))
	}
	ms, err := cold.NewReader(store).Manifests(ctx, now.Add(-time.Minute), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("manifests: %v", err)
	}
	if len(ms) < 3 {
		t.Fatalf("expected the backlog to split across multiple segments (batch cap 2, 5 entries), got %d segment(s)", len(ms))
	}
}
