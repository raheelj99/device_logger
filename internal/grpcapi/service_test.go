package grpcapi

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
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	devicelogv1 "devlog/api/gen/devicelog/v1"
	"devlog/internal/cold"
	"devlog/internal/hot"
	"devlog/internal/ingest"
	"devlog/internal/license"
	"devlog/internal/query"
	"devlog/internal/sign"
	"devlog/internal/telemetry"
)

// emptyStore is a cold.ObjectStore with nothing in it — the query engine then
// serves purely from the hot tier.
type emptyStore struct{}

func (emptyStore) Put(context.Context, string, []byte, string) error { return nil }
func (emptyStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, io.EOF
}
func (emptyStore) List(context.Context, string) ([]string, error) { return nil, nil }

func newTestService(t *testing.T) (*Service, *hot.Store, *sign.Signer) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := hot.New(rdb, time.Hour)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := sign.NewSigner(priv, "k")
	verifier := sign.NewVerifier()
	verifier.Add("k", signer.Public())
	engine := query.New(store, cold.NewReader(emptyStore{}), signer, verifier, 1000, time.Hour)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewService(engine, ingest.NewHub(), store, log), store, signer
}

func storeSigned(t *testing.T, s *hot.Store, signer *sign.Signer, id, device, trace string) {
	t.Helper()
	e := &devicelogv1.LogEntry{
		EntryId: id, DeviceId: device, TraceId: trace, Subsystem: "sanitizer",
		IngestTime: timestamppb.New(time.Now()),
	}
	if err := sign.ChainSign(e, nil, signer); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendWithChain(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

// mockQueryStream captures streamed entries and carries a context.
type mockQueryStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent []*devicelogv1.LogEntry
}

func (m *mockQueryStream) Context() context.Context             { return m.ctx }
func (m *mockQueryStream) Send(e *devicelogv1.LogEntry) error   { m.sent = append(m.sent, e); return nil }

func TestQueryStreamsMatchingEntries(t *testing.T) {
	svc, store, signer := newTestService(t)
	storeSigned(t, store, signer, "id1", "dev1", "job-1")
	storeSigned(t, store, signer, "id2", "dev1", "job-2")

	stream := &mockQueryStream{ctx: context.Background()}
	err := svc.Query(&devicelogv1.QueryRequest{DeviceIds: []string{"dev1"}, TraceId: "job-1"}, stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(stream.sent) != 1 || stream.sent[0].EntryId != "id1" {
		t.Fatalf("expected only job-1's entry, got %d", len(stream.sent))
	}
}

func TestVerifyRangeRequiresDevice(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.VerifyRange(context.Background(), &devicelogv1.VerifyRangeRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestExportAuditReportValidatesAndNotFound(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.ExportAuditReport(context.Background(), &devicelogv1.ExportAuditReportRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for empty trace, got %v", err)
	}
	_, err := svc.ExportAuditReport(context.Background(), &devicelogv1.ExportAuditReportRequest{TraceId: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for unknown trace, got %v", err)
	}
}

func TestGetStatsReportsDevices(t *testing.T) {
	svc, store, signer := newTestService(t)
	storeSigned(t, store, signer, "id1", "dev1", "job-1")
	resp, err := svc.GetStats(context.Background(), &devicelogv1.GetStatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Devices) != 1 || resp.Devices[0].DeviceId != "dev1" || resp.Devices[0].HotEntries != 1 {
		t.Fatalf("unexpected stats: %+v", resp.Devices)
	}
}

func TestAuthorizeRejectsMissingAndBadTokens(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	mgr := license.NewManager(license.NewVerifier(pub), nil, telemetry.NewMetrics(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// No authorization metadata at all.
	if err := authorize(context.Background(), mgr); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated for missing metadata, got %v", err)
	}
	// Present but garbage token.
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer not-a-real-license"))
	if err := authorize(ctx, mgr); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated for bad token, got %v", err)
	}
}

func TestTailMatchAndTsHelpers(t *testing.T) {
	e := &devicelogv1.LogEntry{DeviceId: "dev1", TraceId: "job-1", Severity: devicelogv1.Severity_SEVERITY_INFO}
	if !tailMatch(e, &devicelogv1.TailRequest{}) {
		t.Fatal("empty filter should match everything")
	}
	if tailMatch(e, &devicelogv1.TailRequest{DeviceIds: []string{"other"}}) {
		t.Fatal("device filter should exclude non-matching device")
	}
	if tailMatch(e, &devicelogv1.TailRequest{MinSeverity: devicelogv1.Severity_SEVERITY_ERROR}) {
		t.Fatal("severity floor should exclude INFO")
	}
	if !tsOrZero(nil).IsZero() {
		t.Fatal("nil timestamp should be zero time")
	}
}
