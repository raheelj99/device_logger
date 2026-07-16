// Package query merges the hot and cold tiers into one consistent view and
// implements audit verification over it.
package query

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	devicelogv1 "devlog/api/gen/devicelog/v1"
	"devlog/internal/cold"
	"devlog/internal/hot"
	"devlog/internal/sign"
)

type Engine struct {
	hot             *hot.Store
	cold            *cold.Reader
	signer          *sign.Signer
	verifier        *sign.Verifier
	maxResults      int
	defaultLookback time.Duration
}

func New(h *hot.Store, c *cold.Reader, s *sign.Signer, v *sign.Verifier,
	maxResults int, defaultLookback time.Duration) *Engine {
	return &Engine{hot: h, cold: c, signer: s, verifier: v,
		maxResults: maxResults, defaultLookback: defaultLookback}
}

type Filter struct {
	Devices     []string
	From, To    time.Time
	MinSeverity devicelogv1.Severity
	Subsystem   string
	TraceID     string
	Limit       int
}

// Query returns matching entries from both tiers, deduplicated by entry ID
// (the archiver is at-least-once) and ordered by ingest time.
func (e *Engine) Query(ctx context.Context, f Filter) ([]*devicelogv1.LogEntry, error) {
	if f.To.IsZero() {
		f.To = time.Now()
	}
	if f.From.IsZero() {
		f.From = f.To.Add(-e.defaultLookback)
	}
	limit := f.Limit
	if limit <= 0 || limit > e.maxResults {
		limit = e.maxResults
	}

	seen := map[string]struct{}{}
	var out []*devicelogv1.LogEntry
	collect := func(le *devicelogv1.LogEntry) bool {
		if !match(le, f) {
			return true
		}
		if _, dup := seen[le.EntryId]; dup {
			return true
		}
		seen[le.EntryId] = struct{}{}
		out = append(out, le)
		return len(out) < e.maxResults // hard cap while collecting
	}

	manifests, err := e.cold.Manifests(ctx, f.From, f.To, f.Devices)
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		if err := e.cold.Read(ctx, m, collect); err != nil {
			return nil, err
		}
	}
	devices := f.Devices
	if len(devices) == 0 {
		if devices, err = e.hot.Devices(ctx); err != nil {
			return nil, err
		}
	}
	for _, d := range devices {
		if err := e.hot.Range(ctx, d, f.From, f.To, collect); err != nil {
			return nil, err
		}
	}

	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].IngestTime.AsTime(), out[j].IngestTime.AsTime()
		if ti.Equal(tj) {
			return out[i].EntryId < out[j].EntryId
		}
		return ti.Before(tj)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func match(le *devicelogv1.LogEntry, f Filter) bool {
	t := le.IngestTime.AsTime()
	if t.Before(f.From) || t.After(f.To) {
		return false
	}
	if f.MinSeverity != devicelogv1.Severity_SEVERITY_UNSPECIFIED && le.Severity < f.MinSeverity {
		return false
	}
	if len(f.Devices) > 0 {
		found := false
		for _, d := range f.Devices {
			if le.DeviceId == d {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.Subsystem != "" && le.Subsystem != f.Subsystem {
		return false
	}
	if f.TraceID != "" && le.TraceId != f.TraceID {
		return false
	}
	return true
}

// VerifyRange re-checks every signature and every chain link for a device in
// the window. The first entry's back-link necessarily points outside the
// window and is not checked.
func (e *Engine) VerifyRange(ctx context.Context, device string, from, to time.Time) (*devicelogv1.VerifyRangeResponse, error) {
	entries, err := e.Query(ctx, Filter{Devices: []string{device}, From: from, To: to, Limit: e.maxResults})
	if err != nil {
		return nil, err
	}
	resp := &devicelogv1.VerifyRangeResponse{EntriesChecked: uint64(len(entries))}
	var prevHash []byte
	for i, le := range entries {
		if err := sign.VerifyEntry(le, e.verifier); err != nil {
			resp.Breaks = append(resp.Breaks, &devicelogv1.ChainBreak{EntryId: le.EntryId, Reason: err.Error()})
			continue
		}
		if i > 0 && !bytes.Equal(le.Audit.PrevHash, prevHash) {
			resp.Breaks = append(resp.Breaks, &devicelogv1.ChainBreak{
				EntryId: le.EntryId,
				Reason:  "chain link mismatch: an entry was removed, altered, or reordered before this one",
			})
		}
		prevHash = le.Audit.EntryHash
	}
	resp.Ok = len(resp.Breaks) == 0
	return resp, nil
}

// AuditReport bundles a sanitization job's entries with per-entry
// verification and signs the bundle — the input to a Certificate of
// Sanitization.
func (e *Engine) AuditReport(ctx context.Context, traceID string, from, to time.Time) (*devicelogv1.AuditReport, error) {
	entries, err := e.Query(ctx, Filter{TraceID: traceID, From: from, To: to, Limit: e.maxResults})
	if err != nil {
		return nil, err
	}
	rep := &devicelogv1.AuditReport{
		TraceId:     traceID,
		Entries:     entries,
		GeneratedAt: timestamppb.Now(),
	}
	h := sha256.New()
	valid := len(entries) > 0
	for _, le := range entries {
		if err := sign.VerifyEntry(le, e.verifier); err != nil {
			valid = false
			continue
		}
		rep.SignaturesVerified++
		h.Write(le.Audit.EntryHash)
	}
	rep.AllSignaturesValid = valid
	rep.ReportHash = h.Sum(nil)
	rep.ReportSignature = e.signer.Sign(rep.ReportHash)
	rep.KeyId = e.signer.KeyID()
	return rep, nil
}
