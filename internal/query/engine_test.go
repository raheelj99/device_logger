package query

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
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
	"devlog/internal/sign"
)

// fakeObjStore is an in-memory cold.ObjectStore (no MinIO needed).
type fakeObjStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newFakeObjStore() *fakeObjStore { return &fakeObjStore{m: map[string][]byte{}} }

func (f *fakeObjStore) Put(_ context.Context, k string, d []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[k] = append([]byte(nil), d...)
	return nil
}
func (f *fakeObjStore) Get(_ context.Context, k string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.m[k]
	if !ok {
		return nil, fmt.Errorf("no object %s", k)
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}
func (f *fakeObjStore) List(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for k := range f.m {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

type harness struct {
	engine *Engine
	hot    *hot.Store
	cold   *cold.Writer
	signer *sign.Signer
}

func newHarness(t *testing.T) *harness {
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
	verifier := sign.NewVerifier()
	verifier.Add("test-key", signer.Public())

	objs := newFakeObjStore()
	engine := New(store, cold.NewReader(objs), signer, verifier, 1000, 7*24*time.Hour)
	return &harness{engine: engine, hot: store, cold: cold.NewWriter(objs), signer: signer}
}

// signEntry fills a fully valid, chained, signed entry.
func (h *harness) signEntry(t *testing.T, id, device, msg, trace string, sev devicelogv1.Severity, at time.Time, prev []byte) *devicelogv1.LogEntry {
	t.Helper()
	e := &devicelogv1.LogEntry{
		EntryId:    id,
		DeviceId:   device,
		Subsystem:  "sanitizer",
		Message:    msg,
		TraceId:    trace,
		Severity:   sev,
		IngestTime: timestamppb.New(at),
	}
	if err := sign.ChainSign(e, prev, h.signer); err != nil {
		t.Fatal(err)
	}
	return e
}

func (h *harness) appendHot(t *testing.T, e *devicelogv1.LogEntry) {
	t.Helper()
	if err := h.hot.AppendWithChain(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestQueryMergesTiersDedupesAndSorts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Minute)

	e1 := h.signEntry(t, "id1", "dev1", "cold-only", "job-1", devicelogv1.Severity_SEVERITY_INFO, base, nil)
	e2 := h.signEntry(t, "id2", "dev1", "both-tiers", "job-1", devicelogv1.Severity_SEVERITY_INFO, base.Add(time.Second), e1.Audit.EntryHash)
	e3 := h.signEntry(t, "id3", "dev1", "hot-only", "job-1", devicelogv1.Severity_SEVERITY_ERROR, base.Add(2*time.Second), e2.Audit.EntryHash)

	// e1 + e2 live in cold; e2 + e3 live in hot → e2 is the dedupe case.
	if _, err := h.cold.Write(ctx, []*devicelogv1.LogEntry{e1, e2}); err != nil {
		t.Fatal(err)
	}
	h.appendHot(t, e2)
	h.appendHot(t, e3)

	out, err := h.engine.Query(ctx, Filter{Devices: []string{"dev1"}, From: base.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 deduped entries, got %d: %v", len(out), ids(out))
	}
	// Sorted by ingest time ascending.
	if out[0].EntryId != "id1" || out[2].EntryId != "id3" {
		t.Fatalf("unexpected order: %v", ids(out))
	}
}

func TestQueryAppliesFilters(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Minute)
	h.appendHot(t, h.signEntry(t, "a", "dev1", "info", "job-1", devicelogv1.Severity_SEVERITY_INFO, base, nil))
	h.appendHot(t, h.signEntry(t, "b", "dev1", "err", "job-2", devicelogv1.Severity_SEVERITY_ERROR, base.Add(time.Second), nil))

	// Severity floor excludes the INFO entry.
	out, err := h.engine.Query(ctx, Filter{Devices: []string{"dev1"}, From: base.Add(-time.Hour), MinSeverity: devicelogv1.Severity_SEVERITY_WARN})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].EntryId != "b" {
		t.Fatalf("severity filter failed: %v", ids(out))
	}

	// Trace filter selects one job.
	out, err = h.engine.Query(ctx, Filter{Devices: []string{"dev1"}, From: base.Add(-time.Hour), TraceID: "job-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].EntryId != "a" {
		t.Fatalf("trace filter failed: %v", ids(out))
	}
}

func TestVerifyRangeOKThenDetectsTamper(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Minute)
	e1 := h.signEntry(t, "v1", "dev1", "one", "job", devicelogv1.Severity_SEVERITY_INFO, base, nil)
	e2 := h.signEntry(t, "v2", "dev1", "two", "job", devicelogv1.Severity_SEVERITY_INFO, base.Add(time.Second), e1.Audit.EntryHash)
	h.appendHot(t, e1)
	h.appendHot(t, e2)

	resp, err := h.engine.VerifyRange(ctx, "dev1", base.Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Ok || resp.EntriesChecked != 2 {
		t.Fatalf("expected clean verify, got ok=%v checked=%d breaks=%v", resp.Ok, resp.EntriesChecked, resp.Breaks)
	}

	// Tamper: alter content after signing so the recomputed hash won't match.
	tampered := h.signEntry(t, "v3", "dev2", "orig", "job", devicelogv1.Severity_SEVERITY_INFO, base, nil)
	tampered.Message = "ALTERED"
	h.appendHot(t, tampered)
	resp, err = h.engine.VerifyRange(ctx, "dev2", base.Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Ok || len(resp.Breaks) == 0 {
		t.Fatal("expected tamper to be detected")
	}
}

func TestAuditReportIsSignedAndValid(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Minute)
	e1 := h.signEntry(t, "r1", "dev1", "start", "job-x", devicelogv1.Severity_SEVERITY_INFO, base, nil)
	e2 := h.signEntry(t, "r2", "dev1", "done", "job-x", devicelogv1.Severity_SEVERITY_INFO, base.Add(time.Second), e1.Audit.EntryHash)
	h.appendHot(t, e1)
	h.appendHot(t, e2)

	rep, err := h.engine.AuditReport(ctx, "job-x", base.Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 2 || !rep.AllSignaturesValid || rep.SignaturesVerified != 2 {
		t.Fatalf("bad report: entries=%d valid=%v verified=%d", len(rep.Entries), rep.AllSignaturesValid, rep.SignaturesVerified)
	}
	if len(rep.ReportSignature) == 0 || rep.KeyId != "test-key" {
		t.Fatal("report not signed")
	}
	// The report signature must verify against the signer's public key.
	if !ed25519.Verify(h.signer.Public(), rep.ReportHash, rep.ReportSignature) {
		t.Fatal("report signature does not verify")
	}
}

func ids(es []*devicelogv1.LogEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.EntryId
	}
	return out
}
