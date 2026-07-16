// Package cold is the long-term tier: zstd-compressed segments of
// length-prefixed protobuf entries in an S3-compatible bucket, each paired
// with a small JSON manifest that lets queries prune segments without
// downloading them.
package cold

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"

	devicelogv1 "devlog/api/gen/devicelog/v1"
)

// maxEntryBytes guards DecodeSegment against corrupt or hostile length
// prefixes.
const maxEntryBytes = 64 << 20

type DeviceRange struct {
	Count  int    `json:"count"`
	MinSeq uint64 `json:"min_seq"`
	MaxSeq uint64 `json:"max_seq"`
}

type Manifest struct {
	SegmentKey string                  `json:"segment_key"`
	CreatedAt  time.Time               `json:"created_at"`
	FromMs     int64                   `json:"from_ms"`
	ToMs       int64                   `json:"to_ms"`
	Count      int                     `json:"count"`
	Bytes      int                     `json:"bytes"` // compressed size
	SHA256     string                  `json:"sha256"`
	Devices    map[string]*DeviceRange `json:"devices"`
}

func (m Manifest) Overlaps(from, to time.Time) bool {
	return m.FromMs <= to.UnixMilli() && m.ToMs >= from.UnixMilli()
}

// HasAnyDevice reports whether the segment holds entries for any of the
// requested devices; an empty request matches everything.
func (m Manifest) HasAnyDevice(devices []string) bool {
	if len(devices) == 0 {
		return true
	}
	for _, d := range devices {
		if _, ok := m.Devices[d]; ok {
			return true
		}
	}
	return false
}

// EncodeSegment renders entries as zstd(varint-length || protobuf, ...) and
// builds the matching manifest (SegmentKey left for the writer to fill).
func EncodeSegment(entries []*devicelogv1.LogEntry) ([]byte, Manifest, error) {
	m := Manifest{CreatedAt: time.Now().UTC(), Devices: map[string]*DeviceRange{}}
	if len(entries) == 0 {
		return nil, m, fmt.Errorf("empty segment")
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		return nil, m, err
	}
	var lenBuf [binary.MaxVarintLen64]byte
	m.FromMs, m.ToMs = int64(1<<62), 0
	for _, e := range entries {
		b, err := proto.Marshal(e)
		if err != nil {
			return nil, m, err
		}
		n := binary.PutUvarint(lenBuf[:], uint64(len(b)))
		if _, err := zw.Write(lenBuf[:n]); err != nil {
			return nil, m, err
		}
		if _, err := zw.Write(b); err != nil {
			return nil, m, err
		}

		ms := e.IngestTime.AsTime().UnixMilli()
		m.FromMs = min(m.FromMs, ms)
		m.ToMs = max(m.ToMs, ms)
		m.Count++
		dr := m.Devices[e.DeviceId]
		if dr == nil {
			dr = &DeviceRange{MinSeq: e.Seq, MaxSeq: e.Seq}
			m.Devices[e.DeviceId] = dr
		}
		dr.Count++
		dr.MinSeq = min(dr.MinSeq, e.Seq)
		dr.MaxSeq = max(dr.MaxSeq, e.Seq)
	}
	if err := zw.Close(); err != nil {
		return nil, m, err
	}
	sum := sha256.Sum256(buf.Bytes())
	m.SHA256 = hex.EncodeToString(sum[:])
	m.Bytes = buf.Len()
	return buf.Bytes(), m, nil
}

// DecodeSegment streams entries to fn; fn returns false to stop early.
func DecodeSegment(r io.Reader, fn func(*devicelogv1.LogEntry) bool) error {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	br := bufio.NewReader(zr)
	for {
		n, err := binary.ReadUvarint(br)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("segment corrupt: %w", err)
		}
		if n > maxEntryBytes {
			return fmt.Errorf("segment corrupt: entry of %d bytes", n)
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(br, b); err != nil {
			return fmt.Errorf("segment truncated: %w", err)
		}
		e := &devicelogv1.LogEntry{}
		if err := proto.Unmarshal(b, e); err != nil {
			return fmt.Errorf("segment entry corrupt: %w", err)
		}
		if !fn(e) {
			return nil
		}
	}
}
