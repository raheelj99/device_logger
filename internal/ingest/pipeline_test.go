package ingest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	devicelogv1 "devlog/api/gen/devicelog/v1"
	"devlog/internal/hot"
	"devlog/internal/sign"
	"devlog/internal/telemetry"
)

func newTestPipeline(t *testing.T) (*Pipeline, *hot.Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := hot.New(rdb, time.Hour)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := sign.NewSigner(priv, "test-key")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewPipeline(store, signer, NewHub(), telemetry.NewMetrics(), log), store
}

func marshal(t *testing.T, e *devicelogv1.LogEntry) []byte {
	t.Helper()
	b, err := proto.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readAll(t *testing.T, s *hot.Store, device string) []*devicelogv1.LogEntry {
	t.Helper()
	var out []*devicelogv1.LogEntry
	err := s.Range(context.Background(), device, time.Time{}, time.Time{}, func(e *devicelogv1.LogEntry) bool {
		out = append(out, e)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIngestSignsAndChainsEntries(t *testing.T) {
	p, store := newTestPipeline(t)
	ctx := context.Background()

	for _, msg := range []string{"first", "second"} {
		payload := marshal(t, &devicelogv1.LogEntry{DeviceId: "dev1", Subsystem: "sanitizer", Message: msg})
		if err := p.Ingest(ctx, "dev1", "sanitizer", payload); err != nil {
			t.Fatalf("ingest %q: %v", msg, err)
		}
	}

	got := readAll(t, store, "dev1")
	if len(got) != 2 {
		t.Fatalf("want 2 stored entries, got %d", len(got))
	}
	for _, e := range got {
		if e.Audit == nil || len(e.Audit.Signature) == 0 || len(e.Audit.EntryHash) == 0 {
			t.Fatalf("entry %q missing audit block", e.Message)
		}
		if e.EntryId == "" || e.IngestTime == nil {
			t.Fatalf("server-owned fields not set on %q", e.Message)
		}
	}
	// The chain must link: entry 2's prev_hash == entry 1's entry_hash.
	if string(got[1].Audit.PrevHash) != string(got[0].Audit.EntryHash) {
		t.Fatal("hash chain not linked across entries")
	}
	// The first entry has no predecessor.
	if len(got[0].Audit.PrevHash) != 0 {
		t.Fatal("first entry should have empty prev_hash")
	}
}

func TestIngestRejectsIdentityMismatch(t *testing.T) {
	p, _ := newTestPipeline(t)
	// Payload claims a different device than the authenticated session.
	payload := marshal(t, &devicelogv1.LogEntry{DeviceId: "imposter", Message: "x"})
	err := p.Ingest(context.Background(), "dev1", "sanitizer", payload)
	if err == nil {
		t.Fatal("expected identity-mismatch rejection")
	}
}

func TestIngestDefaultsDeviceAndSubsystem(t *testing.T) {
	p, store := newTestPipeline(t)
	// Producer left device_id/subsystem empty — session values fill them in.
	payload := marshal(t, &devicelogv1.LogEntry{Message: "x"})
	if err := p.Ingest(context.Background(), "dev1", "nav", payload); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, store, "dev1")
	if len(got) != 1 || got[0].DeviceId != "dev1" || got[0].Subsystem != "nav" {
		t.Fatalf("defaulting failed: %+v", got)
	}
}

func TestIngestRejectsNonProtoPayload(t *testing.T) {
	p, _ := newTestPipeline(t)
	// Invalid protobuf wire bytes.
	err := p.Ingest(context.Background(), "dev1", "sanitizer", []byte{0xff, 0xff, 0xff, 0xff})
	if err == nil {
		t.Fatal("expected decode error for non-proto payload")
	}
}

func TestIngestOverwritesProducerAudit(t *testing.T) {
	p, store := newTestPipeline(t)
	// A malicious/confused producer supplies its own audit + entry_id.
	forged := &devicelogv1.LogEntry{
		DeviceId: "dev1",
		EntryId:  "forged-id",
		Audit:    &devicelogv1.Audit{Signature: []byte("fake"), KeyId: "attacker"},
		Message:  "x",
	}
	if err := p.Ingest(context.Background(), "dev1", "sanitizer", marshal(t, forged)); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, store, "dev1")
	if got[0].EntryId == "forged-id" {
		t.Fatal("server accepted producer-supplied entry_id")
	}
	if got[0].Audit.KeyId != "test-key" {
		t.Fatalf("server did not re-sign: key_id=%q", got[0].Audit.KeyId)
	}
}
