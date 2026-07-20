# Component 10 — `internal/query`

> **Role:** merge the hot and cold tiers into one consistent, deduplicated,
> ordered view; filter it; and run tamper verification and signed audit
> reporting over it. | **Source:** `internal/query/engine.go`

**Where this sits in the journey:** this is where the two storage tiers you built
earlier finally become *one thing*. It leans on [06 — hot](06-hot.md) (the Redis
stream store) and [07 — cold](07-cold.md) (the object-store segments) for the
data, [04 — sign](04-sign.md) for the hash/signature primitives, and
[02 — config](02-config.md) for `maxResults`/`defaultLookback`. The *new* Go
territory is data-plumbing in the small: slices and their growth model,
`sort.Slice` with a comparison closure, a closure that accumulates state, a map
used as a set, and a rolling hash pipeline. No goroutines here — just careful,
allocation-aware sequential code.

## What you'll master

- **Go:** **slices in depth** — the `nil` slice, `append` and its growth,
  `len`/`cap`, and reslicing (`out[:limit]`); **`sort.Slice`** with a
  *less-function* closure and a deterministic tie-break; **closures that capture
  and mutate local state** (the `collect` accumulator that returns a `bool` to
  stop early); **a map as a set** (`map[string]struct{}`, the zero-cost
  `struct{}{}` value, and the comma-ok membership test); `bytes.Equal`; a
  **rolling SHA-256** built on `sha256.New()` as an `io.Writer` (`h.Write`,
  `h.Sum(nil)`); `time` windows and defaulting via `IsZero()`; and a **parameter
  object** (`Filter`) instead of a long argument list.
- **Domain:** a unified hot+cold query with server-side filtering; why
  dedupe-by-ULID turns *at-least-once* cold storage into *exactly-once*
  observation; chain-link tamper detection; and a signed, tamper-evident audit
  bundle for a sanitization job.

---

## 1. Orientation

devlogd stores every log entry twice over its lifetime: recently in Redis (the
**hot** tier, doc 06) and, once the archiver drains it, in compressed object-store
segments (the **cold** tier, doc 07). A reader should never have to care which
tier an entry lives in — or that the archiver, being *at-least-once*, may have
copied an entry to cold while it is still hot.

`Engine` is that unifying front door. `Query` reads both tiers over a time
window, applies filters, removes duplicates, orders the result, and caps it.
`VerifyRange` re-checks every signature and every hash-chain link for a device,
reporting the exact point of any tampering. `AuditReport` collects one
sanitization job's entries by trace ID, verifies each, and produces a single
Ed25519-signed bundle — the raw material for a Certificate of Sanitization.

---

## 2. Guided code walkthrough

### 2.1 Package, imports, and the `Engine` struct

```go
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
```

`Engine` holds its collaborators (`hot`, `cold`, `signer`, `verifier`) and two
policy knobs from config. This is the dependency-injection pattern from doc 03:
`New(...)` (omitted — a one-line struct literal) receives everything, so the
engine has no globals and is trivial to test with fakes (which the tests do).

> **Go concept — the three import groups, revisited.** Standard library
> (`bytes`, `sort`, …) sits at the top; a third-party module
> (`google.golang.org/protobuf/...`) and this repo's own packages (`devlog/...`)
> below. `devicelogv1 "devlog/api/gen/devicelog/v1"` is a **named import**: the
> generated package's default name is awkward, so we alias it. The generated
> `*devicelogv1.LogEntry` is the proto type from doc 01.

### 2.2 `Filter` — a parameter object

```go
type Filter struct {
	Devices     []string
	From, To    time.Time
	MinSeverity devicelogv1.Severity
	Subsystem   string
	TraceID     string
	Limit       int
}
```

> **Go concept — a struct as a parameter object.** Rather than
> `Query(ctx, devices, from, to, minSev, subsystem, trace, limit)` — eight
> positional arguments where any two of the same type can be swapped unnoticed —
> we bundle them into one `Filter`. Callers write `Filter{TraceID: "job-x"}` and
> **every unset field takes its zero value** (`nil` slice, zero `time.Time`,
> `""`, `0`). In C++ you might reach for default arguments or an options struct;
> in Go the named-field struct literal *is* the idiom, and it doubles as
> self-documenting keyword arguments. The gotcha: zero must be a *safe* default
> for every field — which is exactly why `Query` re-interprets a zero `From`/`To`
> and a zero `Limit` below.

### 2.3 `Query` — defaulting the window and the limit

```go
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
```

> **Go concept — `time.Time` and the zero-value sentinel.** A `time.Time` is a
> struct, and its **zero value** (`time.Time{}`) represents "no time set" — its
> `IsZero()` method reports exactly that. Go has no `null`/`nil` for value
> types, so the zero value *is* the "absent" marker. Here an unset `To` means
> "now"; an unset `From` means "one default lookback ago". `f.To.Add(-d)`
> subtracts a `time.Duration` by adding its negation. Note `f` is a **copy** —
> `Filter` is passed by value, so mutating `f.To` here cannot surprise the
> caller. That copy is what makes the in-function defaulting clean and local.

`limit <= 0` catches both "unset" (`0`) and nonsense negatives; the `> maxResults`
clause enforces the server's hard ceiling so a client cannot ask for the whole
fleet's history in one call.

### 2.4 The set, the accumulator slice, and the `collect` closure

```go
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
```

This is the heart of the file. Three Go features meet here.

> **Go concept — a map as a set.** Go has no built-in `set` type; the idiom is a
> map whose *value type carries no information*. `map[string]struct{}` maps a key
> to the **empty struct** `struct{}` — a type that occupies **zero bytes**. The
> value literal is the slightly odd-looking `struct{}{}` (the type `struct{}`
> followed by an empty composite literal `{}`). We only ever ask *is this key
> present?*, never *what is its value?*, so storing zero bytes per element is the
> memory-optimal choice. Contrast C++'s `std::unordered_set<std::string>`;
> `map[string]struct{}` is Go's equivalent, and using `map[string]bool` instead
> would waste a byte per entry and invite the bug of checking the value rather
> than membership.

> **Go concept — the comma-ok membership test.** Indexing a map *always*
> succeeds: `seen[k]` returns the value's zero value for a missing key rather
> than throwing. To distinguish "present" from "absent" you use the two-result
> form `v, ok := seen[k]` — `ok` is `true` only if the key exists. Here we don't
> need the value (it's empty anyway), so we write `if _, dup := seen[le.EntryId];
> dup`, discarding the value with the **blank identifier** `_` and reading only
> the boolean. This is the same comma-ok shape you saw for `os.LookupEnv` in
> doc 02 and channel receives elsewhere.

> **Go concept — closures capture variables, not copies.** `collect` is an
> anonymous function assigned to a variable. It refers to `seen`, `out`, `f`, and
> `e` from the enclosing `Query` — and it captures them **by reference**, so
> `out = append(out, le)` mutates the *outer* `out`, and `seen[...] = ...`
> mutates the *outer* map. Every call to `collect`, from either tier, feeds the
> same accumulator. In C++ you'd write `[&](...){...}` and worry about the
> captured references outliving their scope; in Go the compiler's escape analysis
> moves any captured variable to the heap automatically, so there are no dangling
> captures. This is a **callback**: we hand `collect` to the storage layers and
> let *them* drive iteration (doc 06's `Range`, doc 07's `Read` both take a
> `func(*LogEntry) bool`), which keeps the engine ignorant of how each tier
> paginates.

> **Go concept — the `nil` slice and `append`.** `var out []*LogEntry` declares a
> slice with no backing array: it is **`nil`, yet fully usable** — `len(out)` is
> `0` and you can `append` to it immediately. There's no need for
> `make`/`reserve` up front. `append(out, le)` returns a (possibly new) slice
> header; you **must** reassign it (`out = append(...)`) because when the backing
> array is full, `append` allocates a larger one (typically ~2× growth, amortizing
> to O(1) per element) and copies the elements over — the returned header may
> point somewhere new. Forgetting to reassign the result of `append` is a
> classic Go bug. Unlike C++'s `std::vector::push_back`, `append` does not mutate
> in place when it grows; it *returns* the grown slice.

`collect` returns a `bool` meaning "keep going". Returning `true` on a
non-matching or duplicate entry means "skip this one but continue"; the final
`return len(out) < e.maxResults` returns `false` once the accumulator hits the
ceiling, signalling the storage layer to **stop early** rather than materialize a
million rows we'd only discard.

### 2.5 Reading both tiers into the same accumulator

```go
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
```

Cold first: `Manifests` prunes segments by time and device *before* any bytes are
decompressed (cheap index lookup), then each surviving segment is streamed
through `collect`. Then hot: if the caller named no devices we ask the hot store
for the live set, then range each device's stream through the *same* `collect`.

> **Go concept — `range` over a slice, and reusing the callback.** `for _, m :=
> range manifests` yields index and value; we discard the index with `_`. Every
> loop hands `collect` down, so cold entries and hot entries land in one `out`
> and one `seen` — the dedupe happens *across* tiers for free, because the map
> outlives both loops. The `if devices, err = e.hot.Devices(ctx); err != nil`
> form uses `=` (not `:=`) so it assigns the *already-declared* outer `devices`
> and `err` rather than shadowing them in the `if` scope — subtle but deliberate.

### 2.6 `sort.Slice` — ordering with a less-function

```go
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
```

> **Go concept — `sort.Slice` and the less-function closure.** `sort.Slice(s,
> less)` sorts `s` in place using a **less-function** you supply: `less(i, j)`
> returns `true` iff the element at index `i` must come *before* the one at `j`.
> The closure captures `out`, so it can index the slice directly. This is Go's
> answer to C++'s `std::sort(v.begin(), v.end(), cmp)` — but note the contract
> difference: Go's `less` is a strict *less-than*, equivalent to C++'s
> comparator, and returning `true` for equal elements (or an inconsistent
> ordering) yields undefined results. The primary key here is ingest time
> (`ti.Before(tj)`); when two entries share a timestamp we **tie-break on
> `EntryId`**, which is a ULID — lexicographically sortable and unique — so the
> order is *total and deterministic*. Without the tie-break, equal-time entries
> could shuffle between runs (`sort.Slice` is not stable; `sort.SliceStable`
> exists if you need insertion order preserved). `AsTime()` converts the proto
> `timestamppb.Timestamp` to a `time.Time`; `time.Time` comparisons use the
> methods `.Before`/`.After`/`.Equal`, never `<`.

> **Go concept — reslicing to truncate (`out[:limit]`).** `out[:limit]` is a
> **slice expression**: it produces a new slice header sharing the *same backing
> array* but with `len == limit`. It doesn't copy or free anything — the elements
> beyond `limit` are simply no longer visible through `out` (and become
> collectable once the header is the only reference). We collected up to
> `maxResults` during traversal (the hard cap), then trim to the caller's
> `limit ≤ maxResults` here at the end. The gotcha to remember: because the
> backing array is shared, a reslice keeps the whole array alive and a later
> `append` into spare capacity could overwrite data another slice can still see —
> harmless here because `out` is the sole owner and we return immediately.

### 2.7 `match` — the filter predicate

```go
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
```

An unexported predicate: each clause is an early `return false`, and an *unset*
filter field (`""`, empty slice, `SEVERITY_UNSPECIFIED`) is simply skipped —
zero-value-means-"don't filter" again. `Severity` is a proto enum backed by an
integer, so `le.Severity < f.MinSeverity` is a real numeric floor.

### 2.8 `VerifyRange` — `bytes.Equal` and chain links

```go
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
```

`VerifyRange` reuses `Query` (so it verifies the *same* merged, ordered view a
reader sees), then walks the entries checking two independent things:

1. **Signature + content integrity.** `sign.VerifyEntry` (doc 04) recomputes the
   entry's canonical SHA-256, checks it equals the stored `Audit.EntryHash`, and
   verifies the Ed25519 signature over that hash. If content was altered after
   signing, the recomputed hash differs and this fails.
2. **Chain continuity.** Each entry stores `Audit.PrevHash`, which must equal the
   previous entry's `EntryHash`. If an entry was *removed, reordered, or
   swapped*, the link breaks even though each surviving entry's own signature is
   still valid — that's the whole point of a hash chain.

> **Go concept — `bytes.Equal` vs `==`.** Slices are **not comparable with `==`**
> (except to `nil`) — `a == b` is a compile error for `[]byte`. `bytes.Equal(a,
> b)` compares two byte slices element-by-element, returning `true` when both are
> the same length with identical contents (and treating `nil` and empty as
> equal). Use it for hashes and any binary blob. (For *secret* comparisons you'd
> reach for `crypto/subtle.ConstantTimeCompare` to avoid timing leaks; for a
> public hash link `bytes.Equal` is correct and fine.) The `i > 0` guard skips
> the first entry deliberately: its `PrevHash` points at an entry *outside* the
> queried window, so there's nothing in `entries` to compare it against.

Each break is appended to `resp.Breaks` (a slice of `*ChainBreak`) with the exact
`EntryId` and a human-readable reason; `resp.Ok` is simply "no breaks". Note the
`continue` after a signature failure — we record it and move on rather than
aborting, so a single report surfaces *every* problem at once.

### 2.9 `AuditReport` — a rolling SHA-256 and a signed bundle

```go
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
```

> **Go concept — a rolling hash via `hash.Hash` and `io.Writer`.**
> `sha256.New()` returns a `hash.Hash` — an accumulator you feed incrementally.
> Crucially `hash.Hash` **embeds `io.Writer`**, so `h.Write(p)` appends bytes to
> the running digest exactly like writing to a stream (the same `io.Writer` you
> met in doc 07). You never buffer all the input; each `h.Write(le.Audit.EntryHash)`
> folds one entry's hash into the state. `h.Sum(nil)` then returns the final
> digest: the argument is a slice to *append the digest to* (pass `nil` to get a
> fresh `[]byte`), and — a subtle detail — `Sum` does **not** reset the hasher,
> so you could keep writing. This "write repeatedly, `Sum(nil)` at the end"
> shape is the canonical Go hashing pipeline; contrast C++'s
> `SHA256_Init/Update/Final` triple — same idea, but here the hasher *is* just an
> `io.Writer`, so anything that can write to a stream can hash instead.

Because we hash the entry hashes **in the sorted order** produced by `Query`,
`ReportHash` is a commitment to *exactly this set of entries in this order*.
Signing it with `e.signer.Sign` (Ed25519) and stamping `KeyId` makes the whole
bundle **tamper-evident**: anyone with the public key can recompute the hash over
the entries and verify the signature. `valid` starts `true` only if there is at
least one entry (an empty job is not a valid report) and flips to `false` if any
entry fails verification. The tests confirm the emitted `ReportSignature`
verifies against the signer's public key.

---

## 3. Deep dives

### 3.1 Tier merge + dedupe-by-ULID = exactly-once observation

The archiver (doc 08) is deliberately **at-least-once**: it copies a batch to
cold storage and only then acks it out of the hot stream, so a crash between
those two steps means the batch is re-archived — and for a while the same entry
lives in *both* tiers. That's the safe failure mode (never lose data), but it
means a naive union of the two tiers would show duplicates.

`Query` fixes this at read time with the `seen` set. Every entry carries a
**ULID** `EntryId` that is globally unique and assigned once at ingest (doc 09).
The first time `collect` sees a given `EntryId` it records it and keeps the
entry; any later copy — whether from the other tier or a re-archived duplicate —
hits `if _, dup := seen[le.EntryId]; dup` and is dropped. So the *storage*
guarantee is at-least-once but the *observation* guarantee the reader gets is
**exactly-once**. This is the classic distributed-systems move: push the
idempotency to the consumer, keyed on a stable unique id, and you can afford
cheap, lossless, duplicate-prone producers upstream.

The ordering that follows is only possible because we **collect then sort**. A
streaming cross-tier merge-sort would be more memory-frugal, but at fleet-log
scale with a `maxResults` cap the collect-then-sort approach is simpler and
correct, and the streaming gRPC contract (doc 12) leaves room to swap in a
smarter engine later without breaking clients.

### 3.2 Why closures beat returning slices from the tiers

Notice the tiers don't *return* entries — they *call* `collect`. This inversion
matters. If `hot.Range` returned `[]*LogEntry`, the engine would hold each
tier's full result in memory before merging. Instead, the callback lets each tier
stream one entry at a time into a *single shared* accumulator, and lets the
engine abort the traversal (`return false`) the instant it has enough. The
closure is what makes "one accumulator, many producers, early stop" expressible
without any shared interface beyond `func(*LogEntry) bool`. That tiny function
type is the entire coupling between `query`, `hot`, and `cold`.

---

## 4. Idioms & gotchas

- **Always reassign `append`'s result.** `out = append(out, x)`. The returned
  header may point at a newly grown backing array; ignoring the return value is a
  silent data-loss bug.
- **`map[string]struct{}` is the set.** `struct{}{}` is the zero-byte value;
  membership is the comma-ok test `_, ok := m[k]`. Don't use `map[string]bool`
  for a set.
- **Slices aren't `==`-comparable.** Use `bytes.Equal` for `[]byte`; `==` only
  works against `nil`. For secrets, prefer `crypto/subtle.ConstantTimeCompare`.
- **`sort.Slice` needs a strict, total less-func.** Always give a deterministic
  tie-break (here the ULID) or equal keys reorder unpredictably; use
  `sort.SliceStable` only when you must preserve prior order.
- **Zero values are the "unset" markers.** `time.Time{}`/`IsZero()`, empty
  string, `nil` slice, `SEVERITY_UNSPECIFIED`, and `Limit <= 0` all mean "not
  provided" — which is why `Filter{}` is a legal, meaningful query.
- **Reslicing shares memory.** `out[:limit]` doesn't copy; the tail stays alive
  behind the array. Fine when you own the slice; dangerous if another slice
  aliases the same array and you later `append`.
- **`h.Sum(nil)` doesn't reset.** Pass `nil` for a fresh digest; the hasher keeps
  its state afterward if you meant to reuse it.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why is `seen` a `map[string]struct{}` rather than a
   `map[string]bool`, and what exactly does `struct{}{}` denote? What does the
   comma-ok test buy you over plain indexing?
2. **Recall.** In `VerifyRange`, why is the chain-link check guarded by `i > 0`?
   What is `PrevHash` pointing at for the first entry in the window?
3. **Apply.** `Query` collects up to `maxResults` then trims to `limit` with
   `out[:limit]`. Rewrite the trim to *copy* into a right-sized slice and explain
   when the copy is worth the allocation (hint: releasing the large backing
   array).
4. **Apply.** Add a `MessageContains string` field to `Filter` and extend `match`
   to substring-filter on `le.Message`. Which zero value means "don't filter",
   and why must you not touch `Query`?
5. **Extend.** Make the `sort.Slice` order **descending** by ingest time while
   keeping a deterministic tie-break. What must the tie-break do so two
   equal-time entries never swap between runs?
6. **Hero.** `AuditReport` hashes entry hashes in sorted order. Show how a
   verifier, given only `Entries` and the public key, recomputes `ReportHash` and
   checks `ReportSignature` — and explain why reordering two entries would be
   detected even if every individual signature still verifies.

---

## 6. Recap & next

You now know the slice model that underpins every Go collection — the `nil`
slice, `append`'s growth, `len`/`cap`, and reslicing — plus `sort.Slice` with a
tie-broken less-func, closures that accumulate shared state and stop early, a map
used as a zero-cost set with the comma-ok test, `bytes.Equal`, and a streaming
SHA-256 built on `io.Writer`. You also saw the two ideas that make devlogd's
storage trustworthy: dedupe-by-ULID turning at-least-once storage into
exactly-once reads, and hash-chain verification producing a signed,
tamper-evident audit bundle.

**Next:** [11 — broker](11-broker.md), where you embed a third-party interface
via hooks, reason about method sets, and wire TLS into an adapter — the mindset
shift from *writing* logic to *plugging into* someone else's framework.
