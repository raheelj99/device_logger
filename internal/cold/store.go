package cold

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/oklog/ulid/v2"

	devicelogv1 "devlog/api/gen/devicelog/v1"
)

const (
	segmentPrefix  = "segments/"
	manifestPrefix = "manifests/"
	dayLayout      = "2006/01/02"
)

// ObjectStore is the port cold storage talks through; MinioStore is the real
// adapter, tests use an in-memory fake.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]string, error)
}

type MinioStore struct {
	c      *minio.Client
	bucket string
}

func NewMinio(ctx context.Context, endpoint, accessKey, secretKey string, useTLS bool, bucket string) (*MinioStore, error) {
	c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useTLS,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}
	exists, err := c.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("bucket check: %w", err)
	}
	if !exists {
		if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("create bucket: %w", err)
		}
	}
	return &MinioStore{c: c, bucket: bucket}, nil
}

func (m *MinioStore) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := m.c.PutObject(ctx, m.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (m *MinioStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return m.c.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
}

func (m *MinioStore) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for obj := range m.c.ListObjects(ctx, m.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

type Writer struct {
	store ObjectStore
}

func NewWriter(store ObjectStore) *Writer { return &Writer{store: store} }

// Write persists one segment and its manifest. The manifest is written last:
// a segment without a manifest is invisible to queries, so a crash between
// the two writes cannot expose a partial segment.
func (w *Writer) Write(ctx context.Context, entries []*devicelogv1.LogEntry) (Manifest, error) {
	data, m, err := EncodeSegment(entries)
	if err != nil {
		return m, err
	}
	id := ulid.Make().String()
	day := time.UnixMilli(m.FromMs).UTC().Format(dayLayout)
	m.SegmentKey = segmentPrefix + day + "/seg-" + id + ".pb.zst"
	if err := w.store.Put(ctx, m.SegmentKey, data, "application/zstd"); err != nil {
		return m, fmt.Errorf("put segment: %w", err)
	}
	mj, err := json.Marshal(m)
	if err != nil {
		return m, err
	}
	manifestKey := manifestPrefix + day + "/seg-" + id + ".json"
	if err := w.store.Put(ctx, manifestKey, mj, "application/json"); err != nil {
		return m, fmt.Errorf("put manifest: %w", err)
	}
	return m, nil
}

type Reader struct {
	store ObjectStore
}

func NewReader(store ObjectStore) *Reader { return &Reader{store: store} }

// Manifests lists and prunes manifests: only segments overlapping the time
// window and containing a requested device are returned.
func (r *Reader) Manifests(ctx context.Context, from, to time.Time, devices []string) ([]Manifest, error) {
	var out []Manifest
	for day := from.UTC().Truncate(24 * time.Hour); !day.After(to.UTC()); day = day.Add(24 * time.Hour) {
		keys, err := r.store.List(ctx, manifestPrefix+day.Format(dayLayout)+"/")
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			rc, err := r.store.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			var m Manifest
			err = json.NewDecoder(rc).Decode(&m)
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("manifest %s corrupt: %w", key, err)
			}
			if m.Overlaps(from, to) && m.HasAnyDevice(devices) {
				out = append(out, m)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FromMs < out[j].FromMs })
	return out, nil
}

// Read fetches a segment, verifies its checksum against the manifest, and
// streams its entries to fn.
func (r *Reader) Read(ctx context.Context, m Manifest, fn func(*devicelogv1.LogEntry) bool) error {
	rc, err := r.store.Get(ctx, m.SegmentKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read segment %s: %w", m.SegmentKey, err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != m.SHA256 {
		return fmt.Errorf("segment %s checksum mismatch (bucket tampering or corruption)", m.SegmentKey)
	}
	return DecodeSegment(bytes.NewReader(data), fn)
}
