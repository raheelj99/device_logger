package hot

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/timestamppb"

	devicelogv1 "devlog/api/gen/devicelog/v1"
)

func newTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb, 24*time.Hour), mr
}

func testEntry(device string, at time.Time, msg string) *devicelogv1.LogEntry {
	return &devicelogv1.LogEntry{
		EntryId:    msg,
		DeviceId:   device,
		IngestTime: timestamppb.New(at),
		Message:    msg,
		Audit:      &devicelogv1.Audit{EntryHash: []byte(msg)},
	}
}

func TestAppendAndRange(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Minute)
	for i, msg := range []string{"a", "b", "c"} {
		e := testEntry("dev1", base.Add(time.Duration(i)*time.Second), msg)
		if err := s.AppendWithChain(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	err := s.Range(ctx, "dev1", time.Time{}, time.Time{}, func(e *devicelogv1.LogEntry) bool {
		got = append(got, e.Message)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("unexpected range result: %v", got)
	}

	// Time-bounded range should exclude the first entry.
	got = nil
	err = s.Range(ctx, "dev1", base.Add(500*time.Millisecond), time.Time{}, func(e *devicelogv1.LogEntry) bool {
		got = append(got, e.Message)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("time filter failed, got %v", got)
	}
}

func TestChainHead(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	head, err := s.ChainHead(ctx, "dev1")
	if err != nil || head != nil {
		t.Fatalf("expected nil head for new device, got %v, %v", head, err)
	}
	e := testEntry("dev1", time.Now(), "x")
	if err := s.AppendWithChain(ctx, e); err != nil {
		t.Fatal(err)
	}
	head, err = s.ChainHead(ctx, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if string(head) != "x" {
		t.Fatalf("chain head not advanced, got %q", head)
	}
}

func TestDevices(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for _, d := range []string{"beta", "alpha"} {
		if err := s.AppendWithChain(ctx, testEntry(d, time.Now(), "m")); err != nil {
			t.Fatal(err)
		}
	}
	devices, err := s.Devices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 || devices[0] != "alpha" {
		t.Fatalf("unexpected devices: %v", devices)
	}
}

func TestReadGroupAndAck(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.AppendWithChain(ctx, testEntry("dev1", time.Now(), "m1")); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureGroup(ctx, "dev1"); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.ReadGroup(ctx, []string{"dev1"}, "c1", 10, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs["dev1"]) != 1 || msgs["dev1"][0].Entry.Message != "m1" {
		t.Fatalf("unexpected group read: %+v", msgs)
	}
	if err := s.Ack(ctx, "dev1", msgs["dev1"][0].ID); err != nil {
		t.Fatal(err)
	}

	// Everything acked: another read returns nothing new.
	msgs, err = s.ReadGroup(ctx, []string{"dev1"}, "c1", 10, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs["dev1"]) != 0 {
		t.Fatalf("expected empty read after ack, got %+v", msgs)
	}
}
