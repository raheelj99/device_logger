# Component 07 — `internal/cold`

> **Role:** the long-term tier — write immutable, zstd-compressed, checksummed
> segments of log entries to an S3-compatible bucket, each paired with a small
> JSON manifest that lets queries prune without downloading. | **Source:**
> `internal/cold/segment.go`, `internal/cold/store.go`

**Where this sits in the journey:** this is the doc where Go's famous
composability finally clicks. Everything here is built out of a handful of tiny
streaming interfaces — `io.Reader`, `io.Writer`, `io.ReadCloser` — snapped
together like pipe fittings: a compressor wraps a buffer, a varint framer wraps
the compressor, a checksum reads the same bytes. You already know structs,
methods, interfaces, tags, and errors from [02 — config](02-config.md); byte
slices and `crypto/*` from [04 — sign](04-sign.md); and the hot tier's
sentinel/error style from [06 — hot](06-hot.md). Here we go deep on **streaming
I/O**, **binary framing**, **compression**, **`defer`-based cleanup**, and the
**ports & adapters** pattern that keeps business logic ignorant of MinIO.

## What you'll master

- **Go:** the `io.Reader`/`io.Writer`/`io.ReadCloser` interfaces and why they
  compose everything; `bytes.Buffer`, `bufio.Reader`, `io.ReadFull`,
  `io.ReadAll`, `io.NopCloser`; `encoding/binary` **uvarint** length-prefix
  framing (`PutUvarint`/`ReadUvarint`); streaming **compression** with
  `klauspost/compress/zstd`; **`defer`** for cleanup (RAII without destructors);
  **ports & adapters** — an interface as a seam; `crypto/sha256` + `encoding/hex`;
  JSON struct tags; the builtin `min`/`max`; ranging over a channel; struct
  methods as predicates.
- **Domain:** an immutable, checksummed, compressed cold tier; manifest indexing
  and pruning by time and device; write-order safety.

---

## 1. Orientation

The hot tier (doc 06) keeps recent data in Redis for fast reads and short
retention. Eventually that data must move somewhere cheap, durable, and
effectively infinite: object storage (S3, MinIO, GCS). That is the **cold tier**.

`cold` does four things. **`EncodeSegment`** turns a batch of log entries into a
single compressed byte blob plus a `Manifest` describing it. **`Writer.Write`**
puts the blob then the manifest into a bucket. **`Reader.Manifests`** lists
manifests and *prunes* them down to the few that could match a query.
**`Reader.Read`** fetches one segment, verifies its checksum, and streams its
entries back. Segments are **write-once and immutable** — the only integrity
question is "were these exact bytes changed?", answered by a SHA-256 stored in
the manifest.

The package never mentions MinIO in its logic. It talks to an **`ObjectStore`
interface** and lets a concrete adapter supply the bytes. That single seam is
what makes the tier testable and portable.

---

## 2. Guided code walkthrough

### 2.1 The package clause

```go
// Package cold is the long-term tier: zstd-compressed segments of
// length-prefixed protobuf entries in an S3-compatible bucket, each paired
// with a small JSON manifest that lets queries prune segments without
// downloading them.
package cold
```

The doc comment states the whole design in three lines — read it as the contract
the rest of the file keeps.

### 2.2 Imports — the streaming toolbox

```go
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
```

> **Go concept — the `io` family is the lingua franca.** `io.Reader` is one
> method — `Read(p []byte) (n int, err error)` — and `io.Writer` is one method —
> `Write(p []byte) (n int, err error)`. Almost every source or sink of bytes in
> Go satisfies one or both: files, sockets, `bytes.Buffer`, HTTP bodies,
> compressors, hashers. Because the interfaces are tiny and implicit (doc 02),
> anything that reads from an `io.Reader` accepts *all* of them — Unix pipes made
> into a type system. In C++ you'd reach for `std::istream`/`ostream` or a
> template; Go gets the same generality from two one-method interfaces.

> **Go concept — `encoding/binary` and the aliased import.** `encoding/binary`
> gives us the **varint** codec used for framing (§2.5). `devicelogv1 "…/v1"` is
> an **import alias**: the package's real name is awkward, so we rename it
> locally — the same trick you saw in doc 01.

### 2.3 The manifest — JSON structs with tags

```go
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
```

> **Go concept — struct tags, again, now for JSON.** These are the *same* struct
> tags from doc 02, read by a different decoder. `encoding/json` uses `json:"…"`
> to map Go fields to JSON keys. The whole manifest is a plain struct; serializing
> it is one `json.Marshal` call (§2.9). The manifest is deliberately **tiny** — a
> few numbers and a per-device index — so a query can afford to fetch thousands of
> them without touching a single (large) segment.

> **Go concept — `map[string]*DeviceRange` (a map of pointers).** The value type
> is a *pointer* to a struct, not the struct itself. This matters in §2.5: we look
> a device up, and if present we mutate the range **in place** through the pointer.
> A `map[string]DeviceRange` would hand back a *copy* you cannot write into — Go
> map values are not addressable. Storing pointers is the idiom when you update
> aggregates as you iterate.

### 2.4 Struct methods as predicates — `Overlaps` and `HasAnyDevice`

```go
func (m Manifest) Overlaps(from, to time.Time) bool {
	return m.FromMs <= to.UnixMilli() && m.ToMs >= from.UnixMilli()
}

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
```

> **Go concept — value-receiver methods as pure predicates.** Both use a value
> receiver `(m Manifest)` because they only read (the rule from doc 02). They
> encode the *entire* pruning policy as two boolean questions about a manifest —
> "does your time range touch the query's?" and "do you hold any device we want?"
> The classic half-open interval overlap test (`start ≤ otherEnd && end ≥
> otherStart`) is all `Overlaps` needs. `HasAnyDevice` treats an **empty request
> as "match everything"** — a small, deliberate convenience so callers can pass
> `nil` for "all devices". The `_, ok := m.Devices[d]` is the comma-ok map lookup
> from doc 02: we only care *whether* the key exists, so the value is discarded
> with `_`.

### 2.5 `EncodeSegment` — framing + compression in one pass

```go
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
		// …
		n := binary.PutUvarint(lenBuf[:], uint64(len(b)))
		if _, err := zw.Write(lenBuf[:n]); err != nil { /* … */ }
		if _, err := zw.Write(b); err != nil { /* … */ }

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
```

There is a lot of Go packed into this loop. Take it interface by interface.

> **Go concept — `bytes.Buffer` is an in-memory `io.Writer` (and `io.Reader`).**
> `var buf bytes.Buffer` needs no constructor — its **zero value is a ready-to-use
> empty buffer** (doc 02's zero-value rule paying off). It implements `Write`, so
> anything that writes to an `io.Writer` can write into it, and it grows
> automatically. `buf.Bytes()` hands back the accumulated bytes; `buf.Len()` the
> count. It is Go's `std::stringstream`, but usable anywhere a byte sink is
> wanted.

> **Go concept — streaming compression by wrapping a writer.** `zstd.NewWriter(&buf)`
> returns a `*zstd.Encoder` that is *itself* an `io.Writer`. Bytes you `Write` to
> `zw` are compressed and forwarded into `buf`. This is the **decorator pattern
> expressed through interfaces**: the compressor wraps the buffer, and could just
> as easily wrap a file or a socket, because all it requires is *some* `io.Writer`
> underneath. Nothing here holds the whole compressed stream twice — data flows
> through. (We pass `&buf`, a pointer, because the encoder must write into *our*
> buffer, not a copy.)

> **Go concept — uvarint length-prefix framing.** A zstd stream is one
> undifferentiated blob of bytes; on read-back we must know where each protobuf
> message ends. So before every message we write its length. `binary.PutUvarint`
> encodes an unsigned integer as a **variable-length varint** — small numbers take
> one byte, larger ones grow as needed — into a stack array
> `lenBuf [binary.MaxVarintLen64]byte` (the max a uint64 varint can occupy, 10
> bytes). It returns `n`, the bytes actually used, and we write only `lenBuf[:n]`.
> The on-wire shape per entry is `varint(len) ‖ protobuf-bytes`. **Why a length
> prefix?** Protobuf messages are not self-delimiting, so a reader needs an
> explicit frame boundary; length-prefixing is the standard, seek-free way to
> pack a variable-length record stream. (See doc 04 for `proto.Marshal`.)

> **Go concept — the builtin `min`/`max`.** `min(m.FromMs, ms)` and `max(...)`
> are Go 1.21+ *builtins* — no import, generic over any ordered type. We track the
> segment's time span and each device's sequence range as we go. `FromMs` starts
> at a deliberately huge sentinel (`1<<62`) so the first real timestamp always
> wins the `min`; `ToMs` starts at `0` so the first wins the `max`. This is the
> idiomatic replacement for `std::min`/`std::max` — and unlike C's macros, they
> evaluate arguments once.

> **Go concept — `Close` finalizes, and errors matter.** `zw.Close()` **flushes
> and finishes the zstd frame** — a compressor buffers internally, so skipping
> `Close` would truncate the output. We check its error, then read `buf.Bytes()`.
> Only *after* the stream is complete do we hash it. Note this `Close` is *not*
> deferred: we must observe its result and the finished bytes before hashing, so
> it is called explicitly at the right moment.

> **Go concept — hashing with `crypto/sha256` + `encoding/hex`.**
> `sha256.Sum256(buf.Bytes())` returns a fixed `[32]byte` **array** (not a slice).
> `sum[:]` slices the array to feed `hex.EncodeToString`, which renders it as a
> 64-character lowercase hex string stored in the manifest. This checksum is the
> segment's fingerprint; `Reader.Read` recomputes it (§2.11). `crypto/sha256`
> ships with Go — no OpenSSL, matching doc 04's crypto story.

Note the return signature `([]byte, Manifest, error)` — the compressed segment,
its manifest with everything *except* `SegmentKey` filled in (the writer assigns
that), and an error. Three return values, the last an `error`: the shape from
doc 02, extended.

### 2.6 `DecodeSegment` — reading the frames back

```go
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
```

This is `EncodeSegment` run backwards, and it shows why the `io` interfaces are
worth learning.

> **Go concept — accepting `io.Reader`, not a concrete type.** `DecodeSegment`
> takes any `io.Reader`. In production it is fed a `bytes.Reader` over downloaded
> bytes (§2.11); in a test it could be a `bytes.Buffer` or an `os.File`. By
> depending on the *interface*, the decoder is decoupled from where bytes come
> from. This is the same lesson as doc 02's implicit interfaces, now load-bearing.

> **Go concept — decompression wraps a reader.** `zstd.NewReader(r)` returns a
> reader that decompresses `r` on the fly and is *itself* an `io.Reader` — the
> mirror of `NewWriter`. Reads from `zr` pull compressed bytes from `r` and
> hand back plaintext.

> **Go concept — `defer` is RAII without destructors.** `defer zr.Close()`
> schedules `zr.Close()` to run when `DecodeSegment` returns, **no matter which
> `return` fires** — the happy path, an EOF, or any error mid-loop. This is Go's
> answer to C++ RAII: instead of a destructor tied to scope, you write the
> cleanup *next to* the acquisition and Go guarantees it runs on function exit.
> `defer`s run in LIFO order. It is the single most reliable way to avoid leaked
> handles, and you will see it on every `Get`/`Open`/`NewReader` in this package.

> **Go concept — `bufio.Reader` for cheap small reads.** `binary.ReadUvarint`
> reads **one byte at a time** until the varint terminates. Calling the
> decompressor for each single byte would be wasteful, so we wrap it in a
> `bufio.NewReader`, which reads in large chunks and serves small reads from an
> in-memory buffer. `bufio.Reader` also satisfies `io.ByteReader`, the interface
> `ReadUvarint` actually requires. (A `bufio.Writer` is the symmetric batching
> wrapper for writes.)

> **Go concept — `io.EOF` is a normal value, not an exception.** The loop has no
> counter; it reads frames until `binary.ReadUvarint` returns `io.EOF`, the
> sentinel meaning "no more bytes". We test `err == io.EOF` and return `nil`
> (clean end). *Any other* error is genuine corruption. Compare doc 06's
> `errors.Is` sentinels — `io.EOF` is the archetypal Go sentinel error.

> **Go concept — `io.ReadFull` fills the whole slice.** A plain `Read` may return
> *fewer* bytes than requested; `io.ReadFull(br, b)` loops until `b` is completely
> filled or fails. We `make([]byte, n)` — a slice of exactly the framed length —
> then demand exactly that many bytes. A short read here means the segment was
> **truncated**, reported as such. The `maxEntryBytes` guard (`64 << 20`, 64 MiB)
> runs *before* the allocation so a corrupt or hostile length prefix cannot make
> us allocate gigabytes.

> **Go concept — a `func` value as a callback (streaming, not buffering).**
> `fn func(*devicelogv1.LogEntry) bool` is a **function value** (doc 03).
> `DecodeSegment` calls it once per entry and hands the entry over instead of
> building a giant `[]*LogEntry`. Returning `false` from `fn` stops decoding early
> — the query layer uses this to bail once it has enough results. This keeps
> memory bounded even for huge segments: one entry is live at a time.

### 2.7 The port — `ObjectStore` interface

```go
// ObjectStore is the port cold storage talks through; MinioStore is the real
// adapter, tests use an in-memory fake.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]string, error)
}
```

> **Go concept — ports & adapters (the hexagonal seam).** This interface is a
> **port**: the abstract capability cold storage needs — put a blob, get a blob,
> list keys — with *no mention of MinIO, S3, or HTTP*. The concrete `MinioStore`
> is an **adapter** that implements it. `Writer` and `Reader` hold an
> `ObjectStore`, never a `*minio.Client`, so the storage backend is a pluggable
> detail. Swap MinIO for GCS or Azure by writing a new adapter; the pipeline is
> untouched. Tests inject an **in-memory fake**. In C++ you would express this with
> a pure-virtual base class and inheritance; Go expresses it with an implicit
> interface and *no* inheritance — an adapter satisfies the port simply by having
> the right methods.

> **Go concept — `io.ReadCloser` composes two interfaces.** `Get` returns
> `io.ReadCloser`, which is literally `interface { Reader; Writer-less Closer }` —
> an embedded `io.Reader` plus an `io.Closer` (`Close() error`). It models "a
> stream you read *and must close*", exactly like an open S3 object or file. The
> caller reads bytes then `Close()`s (via `defer`) to release the connection.
> Interface **embedding** — building `ReadCloser` from `Reader` + `Closer` — is
> how Go composes small interfaces into larger ones without inheritance.

### 2.8 The adapter — `MinioStore`

```go
type MinioStore struct {
	c      *minio.Client
	bucket string
}

func NewMinio(ctx context.Context, endpoint, accessKey, secretKey string, useTLS bool, bucket string) (*MinioStore, error) {
	c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useTLS,
	})
	// …
	exists, err := c.BucketExists(ctx, bucket)
	// …
	if !exists {
		if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("create bucket: %w", err)
		}
	}
	return &MinioStore{c: c, bucket: bucket}, nil
}
```

`NewMinio` builds the real client, and auto-creates the bucket if missing so a
fresh deployment just works. Credentials arrive as parameters — never hard-coded;
in configuration they are placeholders like `YOUR_ACCESS_KEY` until the
environment supplies the real values.

```go
func (m *MinioStore) Put(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := m.c.PutObject(ctx, m.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (m *MinioStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return m.c.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
}
```

> **Go concept — `bytes.NewReader` adapts a `[]byte` into an `io.Reader`.**
> `PutObject` wants a stream, but we hold a `[]byte`. `bytes.NewReader(data)`
> wraps the slice as a `*bytes.Reader` — a read-only `io.Reader` (and seeker) over
> those bytes, no copy. This is the everyday bridge from "bytes in hand" to "the
> streaming API a library expects".

> **Go concept — implementing the port by matching signatures.** `MinioStore`
> never says `implements ObjectStore`. Because its methods match the port's
> signatures exactly, it *is* an `ObjectStore`. `GetObject` returns a
> `*minio.Object`, which already satisfies `io.ReadCloser`, so `Get` returns it
> directly — the adapter is a thin translation layer.

```go
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
```

> **Go concept — ranging over a channel.** `ListObjects` returns a **channel**
> (`<-chan ObjectInfo`), streaming results as it pages the bucket. `for obj :=
> range ch` receives values until the channel is **closed**, then the loop ends
> naturally — the same `range` keyword as maps and slices, but here it *blocks*
> for each next value. Each result carries its own `obj.Err` (a per-item error, an
> API design channels invite); we check it and abort on the first failure. This is
> your first taste of channels; doc 09 makes them central. `var keys []string`
> starts as a `nil` slice, and `append` grows it — appending to `nil` is perfectly
> legal in Go.

### 2.9 `Writer.Write` — manifest written last

```go
func NewWriter(store ObjectStore) *Writer { return &Writer{store: store} }

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
```

> **Go concept — constructor functions and dependency injection.**
> `NewWriter(store ObjectStore)` takes the *port*, not a concrete store, and
> stashes it. `Writer` cannot know or care whether it is talking to MinIO or a
> fake — the dependency is injected (doc 03). This is what makes the whole tier
> unit-testable without a running bucket.

The **ordering is the correctness argument**: the segment blob is `Put` first,
the manifest (via `json.Marshal`) *last*. Because queries discover segments only
through manifests (§2.10), a crash *between* the two writes leaves an orphan
segment that **no query can ever see** — there is no partial or half-written
segment visible to readers. Keys are laid out by day (`segments/2006/01/02/…`
via `Format(dayLayout)`, Go's reference-time layout you met in doc 05), so
listing a day is cheap. The ULID gives each segment a unique, sortable name.

### 2.10 `Reader.Manifests` — pruning

```go
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
```

This is manifest indexing in action: iterate day-by-day over the query window
(`day.Add(24*time.Hour)` walks days; `Truncate` snaps to midnight), list only
that day's manifest prefix, decode each small JSON manifest, and keep only those
that both `Overlaps` the window and `HasAnyDevice` — the two predicates from §2.4.
Segments themselves are never touched here; a query over a narrow window reads a
handful of tiny manifests instead of scanning the whole bucket.

> **Go concept — `json.NewDecoder(rc).Decode` streams from a reader.** Rather
> than read all bytes then unmarshal, `json.NewDecoder` decodes straight from the
> `io.ReadCloser` — again, the interface is the only requirement. Note `rc.Close()`
> is called explicitly right after decoding (not deferred) because we are in a
> loop: deferring inside a loop would pile up every open stream until the function
> returns. When a `Close` belongs to *one iteration*, close it in that iteration.

> **Go concept — `sort.Slice` with a less-func.** `sort.Slice(out, func(i, j int)
> bool { … })` sorts in place using an inline comparison closure (returns "is i
> before j?"). We return manifests oldest-first by `FromMs`, giving the query
> layer time-ordered input. This is Go's general-purpose sort; the closure is a
> function value, same concept as the decode callback.

### 2.11 `Reader.Read` — verify, then stream

```go
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
```

> **Go concept — `io.ReadAll` drains a reader to a slice, `defer rc.Close()`
> guards it.** `io.ReadAll(rc)` reads the `io.ReadCloser` to EOF into a `[]byte`.
> The `defer rc.Close()` on the line above ensures the stream is released whether
> the read, the checksum, or the decode fails — RAII cleanup for the whole
> function in one line.

The integrity check is the payoff of storing `SHA256` in the manifest: recompute
the hash over the downloaded bytes and compare. A mismatch means **the stored
bytes changed** — corruption or tampering — and we refuse to decode, failing
loudly. Only verified bytes reach `DecodeSegment`, wrapped as a `bytes.Reader` so
the decoder gets its `io.Reader`. The same `fn` callback streams entries out,
one at a time.

---

## 3. Deep dives

### 3.1 The storage port: one interface, three implementations

`ObjectStore` is the doc's signature idea. `Writer` and `Reader` depend *only* on
that interface, so three realities coexist behind one type:

- **`MinioStore`** — the production adapter over a real S3/MinIO bucket.
- **A GCS or Azure adapter** — hypothetical, but adding one is *only* writing
  three methods; not a line of `Writer`/`Reader` changes. That is the whole point
  of ports & adapters: the volatile detail (which cloud) lives at the edge.
- **An in-memory fake** — for tests. It is roughly a `map[string][]byte` guarded
  by a mutex, with `Get` returning the bytes as an `io.ReadCloser`.

> **Go concept — `io.NopCloser` supplies a no-op `Close`.** The fake's `Get` must
> return an `io.ReadCloser`, but `bytes.NewReader(data)` is only an `io.Reader` —
> it has no `Close`. `io.NopCloser(bytes.NewReader(data))` wraps it, adding a
> `Close() error` that does nothing and returns `nil`. This is the standard way to
> hand a plain reader to an API that demands a closer — common exactly when
> building test doubles, where there is no real resource to release.

Because the fake and MinIO are interchangeable, the entire encode/write/prune/
read pipeline is exercised in memory, in microseconds, with no network — while
production swaps in the real bucket by changing one constructor argument. The
interface is the seam that makes both true at once.

### 3.2 The segment format, end to end

A segment is a self-describing, integrity-checked, compressed record stream, and
every layer earns its place:

1. **Per-entry framing** — `varint(len) ‖ protobuf(entry)`, repeated. The varint
   prefix makes an otherwise opaque protobuf stream *iterable*: the reader knows
   exactly how many bytes the next message occupies, with no separators to escape
   and no need to seek.
2. **Whole-stream compression** — the framed bytes flow through `zstd.NewWriter`,
   so compression sees the *concatenation* of all entries and exploits redundancy
   across them (far better than compressing each entry alone). Decode reverses it:
   `zstd.NewReader` → `bufio` → read varint → `io.ReadFull` → `proto.Unmarshal`.
3. **Integrity** — SHA-256 over the *compressed* bytes, stored in the manifest,
   re-verified on every read. Segments are write-once; the hash detects any later
   change.
4. **Manifest indexing** — the segment is opaque and large; the manifest is tiny
   and queryable. Pruning happens entirely at the manifest layer, so a query pays
   for a few small JSON reads, not a bucket scan.
5. **Write order** — segment first, manifest last, so a crash can only ever
   produce an invisible orphan, never a corrupt-looking readable segment.

The through-line is the `io` interfaces: buffer, compressor, framer, hasher, and
object stream are all just `Reader`s and `Writer`s snapped together. Learn to see
a pipeline as a chain of these, and Go's whole I/O world opens up.

---

## 4. Idioms & gotchas

- **`defer Close()` for whole-function cleanup; explicit `Close()` inside loops.**
  `DecodeSegment` and `Read` defer; `Manifests` closes each manifest stream in
  the iteration. Deferring inside a loop leaks handles until the function returns.
- **Check `Close()` on writers.** `zw.Close()` flushes the final compressed
  frame; ignoring it can silently truncate output. Closing a *reader* is about
  releasing resources, so its error is often less critical.
- **Guard length prefixes before allocating.** `n > maxEntryBytes` runs before
  `make([]byte, n)` — never trust a size read off the wire.
- **`io.EOF` is expected, everything else is corruption.** Distinguish the clean
  end (`err == io.EOF`) from genuine failure; treat them oppositely.
- **`sha256.Sum256` returns an array; slice it with `sum[:]`.** Arrays and slices
  are different types in Go; hashers hand back fixed arrays.
- **Depend on `io.Reader`/`ObjectStore`, not concretes.** Every function here
  takes the smallest interface that does the job — the reason the package tests
  without a bucket.
- **Map values that you mutate should be pointers.** `map[string]*DeviceRange`
  lets you update ranges in place; a value map would return uneditable copies.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why is the manifest written *after* the segment, and what exactly
   goes wrong if the order is swapped?
2. **Recall.** `DecodeSegment` returns `nil` on `io.EOF` but an error on any
   other read failure. Why is that distinction correct rather than a bug?
3. **Apply.** Implement the in-memory fake `ObjectStore` (map + mutex). Which
   standard helper do you need so `Get` can return an `io.ReadCloser` over a
   `[]byte`, and why?
4. **Apply.** Add a `contentEncoding` field to `Manifest` (JSON tag included) and
   populate it in `EncodeSegment`. Which decoder reads it back, and what would a
   typo in the tag do?
5. **Extend.** `Read` verifies the checksum after `io.ReadAll`, buffering the
   whole segment. Sketch a *streaming* verifier using `io.TeeReader` that hashes
   while decoding. What integrity guarantee do you lose by only checking the sum
   *after* you've already streamed entries out?
6. **Hero.** Write a `GCSStore` adapter skeleton satisfying `ObjectStore`. How
   many lines of `Writer`/`Reader` must change? (Answer: zero — explain why.)

---

## 6. Recap & next

You have seen Go's I/O philosophy in full: tiny `io.Reader`/`io.Writer`/
`io.ReadCloser` interfaces compose into pipelines — buffer, compressor, varint
framer, hasher, object stream — where each stage only needs "some reader" or
"some writer" beneath it. You met varint length-prefix framing and why a stream
of protobufs needs it, streaming zstd, `defer` as RAII, SHA-256 integrity, JSON
manifests, ranging a channel, and the ports & adapters pattern that lets one
`ObjectStore` interface stand in for MinIO, GCS, or an in-memory fake.

**Next:** [08 — archive](08-archive.md), where a long-running background loop
drives this cold tier — batching hot-tier entries, calling `Writer.Write`, and
acking only after durability, all under `context` cancellation and graceful
shutdown.
