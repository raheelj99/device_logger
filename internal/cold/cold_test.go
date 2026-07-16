package cold

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	devicelogv1 "devlog/api/gen/devicelog/v1"
)

// fakeStore is an in-memory ObjectStore for tests.
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

func entryAt(device string, at time.Time, seq uint64) *devicelogv1.LogEntry {
	return &devicelogv1.LogEntry{
		EntryId:    fmt.Sprintf("%s-%d", device, seq),
		DeviceId:   device,
		IngestTime: timestamppb.New(at),
		Seq:        seq,
		Message:    "m",
	}
}

func TestSegmentRoundTrip(t *testing.T) {
	now := time.Now()
	in := []*devicelogv1.LogEntry{
		entryAt("dev1", now, 1),
		entryAt("dev1", now.Add(time.Second), 2),
		entryAt("dev2", now.Add(2*time.Second), 7),
	}
	data, m, err := EncodeSegment(in)
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != 3 || m.Devices["dev1"].Count != 2 || m.Devices["dev2"].MinSeq != 7 {
		t.Fatalf("bad manifest: %+v", m)
	}
	var out []*devicelogv1.LogEntry
	if err := DecodeSegment(bytes.NewReader(data), func(e *devicelogv1.LogEntry) bool {
		out = append(out, e)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[0].EntryId != "dev1-1" || out[2].DeviceId != "dev2" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestWriterReaderWithPruning(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	w := NewWriter(store)
	r := NewReader(store)
	now := time.Now().UTC()

	if _, err := w.Write(ctx, []*devicelogv1.LogEntry{entryAt("dev1", now, 1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(ctx, []*devicelogv1.LogEntry{entryAt("dev2", now, 2)}); err != nil {
		t.Fatal(err)
	}

	// Device pruning: only dev1's segment should be selected.
	ms, err := r.Manifests(ctx, now.Add(-time.Hour), now.Add(time.Hour), []string{"dev1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("expected 1 manifest after device pruning, got %d", len(ms))
	}
	var got []string
	if err := r.Read(ctx, ms[0], func(e *devicelogv1.LogEntry) bool {
		got = append(got, e.EntryId)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "dev1-1" {
		t.Fatalf("unexpected entries: %v", got)
	}

	// Time pruning: a window in the past selects nothing.
	ms, err = r.Manifests(ctx, now.Add(-3*time.Hour), now.Add(-2*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 0 {
		t.Fatalf("expected 0 manifests after time pruning, got %d", len(ms))
	}
}

func TestChecksumMismatchDetected(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	w := NewWriter(store)
	m, err := w.Write(ctx, []*devicelogv1.LogEntry{entryAt("dev1", time.Now(), 1)})
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte of the stored segment.
	store.mu.Lock()
	store.objects[m.SegmentKey][0] ^= 0xFF
	store.mu.Unlock()

	err = NewReader(store).Read(ctx, m, func(*devicelogv1.LogEntry) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
}
