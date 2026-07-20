# Component 06 — `internal/hot`

> **Role:** the short-term storage tier — one Redis Stream per device that
> doubles as the durable buffer (WAL) the archiver drains into cold storage. |
> **Source:** `internal/hot/store.go`

**Where this sits in the journey:** your first component that talks to an
external system through a **third-party client library**, and your first taste
of **transactions**, **callbacks**, and **sentinel errors** in anger. The Go
foundations — structs, methods, pointers, error values, the `x, err :=` shape —
are all from [02 — config](02-config.md); the `proto.Marshal`/`Unmarshal`
byte-slice work is from [04 — sign](04-sign.md); the "return a sentinel, test
it with `errors.Is`" pattern was previewed in [05 — license](05-license.md) and
gets its full treatment here. Prerequisites: docs 02, 04, 05.

## What you'll master

- **Go:** driving a **third-party client** (`github.com/redis/go-redis/v9`) —
  constructing a `*redis.Client`, and its **typed result wrappers** with
  `.Result()` / `.Err()`; **sentinel errors** and `errors.Is` (`redis.Nil`);
  **transactions via pipelines** (`TxPipeline`, `pipe.Exec`); `strconv` for
  **integer↔string** formatting (`FormatInt`) and stream-ID construction;
  **higher-order functions / callbacks** (`fn func(*LogEntry) bool` with
  early-stop); pre-sized slices with `make([]T, 0, n)` and `append`;
  `sort.Strings`; `strings.TrimPrefix`; exported result structs (`Raw`,
  `DeviceStat`).
- **Domain:** the hot tier and its **WAL / durable-buffer** role; Redis Streams
  and their time-encoding IDs; **consumer groups** for at-least-once delivery
  and crash recovery; `hot_retention` trimming; per-device chain-head storage;
  cheap stats.

---

## 1. Orientation

devlogd receives signed log entries from devices in the field and must (a)
answer "give me device X's logs between these times" quickly, and (b) never
lose an entry on its way to permanent cold storage. The **hot tier** does both.
Every device gets its own **Redis Stream** — an append-only, ordered log inside
Redis. Writing an entry is a fast, durable `XADD`; recent reads are served
straight from Redis; and the same stream acts as a **write-ahead log (WAL)**:
the archiver consumes from it, and until an entry is acknowledged, Redis keeps
it. A crash mid-archive loses nothing.

The public surface is the `Store` type: `AppendWithChain` (write), `Range`
(time-bounded read), `ChainHead`/`Devices`/`Stats` (metadata), `Trim` +
`RunJanitor` (retention), and the consumer-group quartet `EnsureGroup` /
`ReadGroup` / `Ack` / `ClaimStale` used by the archiver ([08](08-archive.md)).

---

## 2. Guided code walkthrough

### 2.1 Package doc and imports

```go
// Package hot is the short-term tier: one Redis Stream per device. Streams
// double as the durable buffer (WAL) the archiver drains into cold storage,
// so an entry acknowledged to the producer survives a devlogd crash.
package hot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	devicelogv1 "devlog/api/gen/devicelog/v1"
)
```

> **Go concept — a third-party client, imported like any other package.**
> `github.com/redis/go-redis/v9` is an external module pinned in `go.mod`
> (doc 01). You use it exactly like the standard library: import the path,
> reference it by its last element (`redis.Client`, `redis.Nil`). The trailing
> `/v9` is Go's **semantic-import versioning** — a major version ≥ 2 is part of
> the import path, so v8 and v9 could even coexist in one build. There is no
> header to include and no link step: `go build` fetches and compiles it. The
> third import group (`devicelogv1 "..."`) is *this* module's generated proto
> package, given a short local alias.

### 2.2 Keys, the sentinel group name, and the `Store`

```go
const (
	devicesKey = "devlog:devices"
	chainKey   = "devlog:chain"
	entryField = "e"

	// Group is the archiver's consumer group; unacked entries are reclaimed
	// after a crash, giving at-least-once delivery to cold storage.
	Group = "archiver"
)

func streamKey(device string) string { return "devlog:stream:" + device }

type Store struct {
	rdb       *redis.Client
	retention time.Duration
}

func New(rdb *redis.Client, retention time.Duration) *Store {
	return &Store{rdb: rdb, retention: retention}
}
```

> **Go concept — constructing and holding the client.** `*redis.Client` is a
> connection *pool*, not a single socket; it is safe for concurrent use and is
> created once (in the composition root, doc 14) with
> `redis.NewClient(&redis.Options{Addr: ...})` and injected here. `Store` simply
> stores the pointer — this is the **dependency injection** you met in doc 03:
> `hot` never reaches for a global, it receives its collaborator. `New` returns
> `*Store` (pointer) so every caller shares the one pool.

`streamKey` is an unexported helper turning a device ID into its Redis key;
`devicesKey`/`chainKey` are the two shared keys (a Set of known devices and a
Hash of chain heads). `entryField` (`"e"`) is the single field name each stream
message uses to hold the marshaled entry bytes.

### 2.3 The exported result structs

```go
// Raw is a stream message: the decoded entry plus its stream ID for acking.
type Raw struct {
	ID    string
	Entry *devicelogv1.LogEntry
}
```

> **Go concept — a small exported struct as a return type.** `Raw` pairs the
> Redis message **ID** (needed later to `Ack` it) with the decoded protobuf
> `*LogEntry`. Both fields are capitalised, so they're **exported** (doc 02).
> This is idiomatic Go: rather than returning two parallel slices or a
> `map[string]*LogEntry`, you define a tiny value type that names its parts.
> `DeviceStat` (§2.9) does the same for stats.

### 2.4 `AppendWithChain` — a transaction

```go
func (s *Store) AppendWithChain(ctx context.Context, e *devicelogv1.LogEntry) error {
	b, err := proto.Marshal(e)
	if err != nil {
		return err
	}
	// Stream IDs carry the ingest time (ms) so time-range reads and
	// retention trimming operate directly on IDs.
	id := strconv.FormatInt(e.IngestTime.AsTime().UnixMilli(), 10) + "-*"
	pipe := s.rdb.TxPipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{Stream: streamKey(e.DeviceId), ID: id, Values: map[string]any{entryField: b}})
	pipe.HSet(ctx, chainKey, e.DeviceId, string(e.Audit.EntryHash))
	pipe.SAdd(ctx, devicesKey, e.DeviceId)
	_, err = pipe.Exec(ctx)
	return err
}
```

> **Go concept — `strconv` for integer↔string.** Redis stream IDs are strings
> of the form `"<ms>-<seq>"`. `strconv.FormatInt(n, 10)` renders an `int64` in
> base 10 as a string (the inverse is `strconv.ParseInt`). Go will **not**
> implicitly convert a number to a string — `string(n)` would (mis)interpret
> `n` as a Unicode code point — so `strconv` is the correct, explicit tool.
> Here we format the ingest time in **milliseconds** and append `"-*"`, telling
> Redis "use this millisecond timestamp, auto-assign the sequence". The result
> is that every ID *encodes its own ingest time* — the keystone of §3.1.

> **Go concept — transactions via a pipeline.** `s.rdb.TxPipeline()` opens a
> Redis transaction (`MULTI`). The three `pipe.X...` calls **queue** commands —
> they don't hit Redis yet — and `pipe.Exec(ctx)` sends them wrapped in
> `MULTI/EXEC`, so Redis runs all three **atomically**: either the stream append,
> the chain-head update, and the device registration all apply, or none do. That
> atomicity is the whole point of the method's name: the hash chain (doc 04)
> must advance in lockstep with the stored entry, or a reader could see an entry
> whose hash isn't yet recorded (or vice versa). Note the doc comment states the
> invariant — "they must move together to keep the chain linear."

> **Go concept — `map[string]any` and `context.Context`.** `Values:
> map[string]any{entryField: b}` maps to the empty interface `any` (Go's
> `interface{}`) because Redis field values are heterogeneous; here the one value
> is the `[]byte` from `proto.Marshal`. Every call takes a `context.Context`
> first — the standard carrier for cancellation/deadlines (wired in doc 08); the
> client aborts the round-trip if `ctx` is cancelled. `pipe.Exec` returns
> `([]redis.Cmder, error)`; we discard the per-command results with `_` and
> surface only the error.

### 2.5 `ChainHead` — a sentinel error via `errors.Is`

```go
func (s *Store) ChainHead(ctx context.Context, device string) ([]byte, error) {
	v, err := s.rdb.HGet(ctx, chainKey, device).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(v), nil
}
```

> **Go concept — typed result wrappers (`.Result()` / `.Err()`).** go-redis does
> not return `(value, error)` directly. Each verb returns a **command object** —
> `HGet` returns a `*redis.StringCmd` — that carries both the result and the
> error. You unpack it with `.Result()` (giving `(string, error)`) when you want
> the value, or `.Err()` (just `error`) when you only care whether it worked
> (see `Trim`, `Ack`). This wrapper design lets one type expose typed getters
> like `.Val()`, `.Int64()`, etc.

> **Go concept — sentinel errors and `errors.Is`.** `redis.Nil` is a
> **sentinel**: a single, package-level error value (`var Nil = ...`) that means
> exactly "key/field does not exist". You test for it with
> `errors.Is(err, redis.Nil)` rather than `err == redis.Nil`, because `errors.Is`
> also unwraps errors that were wrapped with `%w` (doc 02) — it walks the chain
> looking for a match. This is the pattern doc 05 introduced with its own
> sentinels; go-redis uses the same idiom. Here "field missing" is **not** an
> error to the caller — a device that never logged simply has no chain head — so
> we translate it into `(nil, nil)`. Any *other* error propagates.

### 2.6 `Devices` — a Set plus `sort.Strings`

```go
func (s *Store) Devices(ctx context.Context) ([]string, error) {
	devices, err := s.rdb.SMembers(ctx, devicesKey).Result()
	if err != nil {
		return nil, err
	}
	sort.Strings(devices)
	return devices, nil
}
```

`SMembers` reads the Redis Set of device IDs. A Set has **no order**, so we
sort for deterministic output.

> **Go concept — `sort.Strings`.** `sort.Strings(devices)` sorts a `[]string`
> **in place** (it mutates the slice, returns nothing), ascending. It's a
> convenience wrapper over the general `sort.Slice`/`sort.Sort`. Slices are
> reference-like — the header is passed by value but points at the same backing
> array — so the caller sees the sorted result. This determinism is what lets
> the test assert `devices[0] == "alpha"`.

### 2.7 `Range` — a streaming callback with early-stop

```go
func (s *Store) Range(ctx context.Context, device string, from, to time.Time, fn func(*devicelogv1.LogEntry) bool) error {
	start, end := "-", "+"
	if !from.IsZero() {
		start = strconv.FormatInt(from.UnixMilli(), 10)
	}
	if !to.IsZero() {
		end = strconv.FormatInt(to.UnixMilli(), 10)
	}
	for {
		msgs, err := s.rdb.XRangeN(ctx, streamKey(device), start, end, 1000).Result()
		if err != nil {
			return err
		}
		for _, m := range msgs {
			e, err := decode(m)
			if err != nil {
				return err
			}
			if !fn(e) {
				return nil
			}
		}
		if len(msgs) < 1000 {
			return nil
		}
		start = "(" + msgs[len(msgs)-1].ID // exclusive resume
	}
}
```

> **Go concept — higher-order functions (callbacks).** The last parameter,
> `fn func(*devicelogv1.LogEntry) bool`, is a **function value** — a first-class
> function passed as an argument, exactly like the closures in doc 03. `Range`
> doesn't build and return a giant slice of every match (which could be huge);
> it **calls `fn` once per entry** as it streams them. The caller supplies the
> body — accumulate, count, write to a socket — without `Range` knowing or
> caring. This is the callback / visitor pattern, and in C++ it's what you'd
> reach `std::function` or a template functor for.

> **Go concept — early-stop via the return value.** `fn` returns a `bool`:
> `true` means "keep going", `false` means "I've seen enough, stop now". When
> `!fn(e)`, `Range` returns immediately. This gives the caller a clean
> break-out — think LIMIT clauses or "first match" — without exceptions or
> sentinel out-parameters. It's the Go analogue of returning `false` from an
> STL-style callback to halt iteration.

The rest is a **paginated `XRANGE` loop**: `"-"`/`"+"` are Redis's smallest/
largest possible IDs; when `from`/`to` are set we substitute millisecond
timestamps (again via `strconv`). `XRangeN(..., 1000)` fetches up to 1000
messages; if fewer come back we're done, otherwise we resume from `"(" + lastID`
— the leading `(` makes the bound **exclusive** so we don't re-read the last
message. `time.Time.IsZero()` detects the caller's "no bound" default (the zero
value from doc 02).

### 2.8 `Trim` and `RunJanitor` — retention

```go
func (s *Store) Trim(ctx context.Context) error {
	devices, err := s.Devices(ctx)
	if err != nil {
		return err
	}
	minID := strconv.FormatInt(time.Now().Add(-s.retention).UnixMilli(), 10)
	for _, d := range devices {
		if err := s.rdb.XTrimMinID(ctx, streamKey(d), minID).Err(); err != nil {
			return fmt.Errorf("trim %s: %w", d, err)
		}
	}
	return nil
}
```

Because IDs encode ingest time, retention is trivial: compute the cutoff
millisecond (`now − hot_retention`), format it, and `XTrimMinID` drops every
message with a smaller ID on each device's stream. We use `.Err()` here (we
don't care how many were trimmed) and wrap failures with the device name via
`%w`. `RunJanitor` (not re-listed) runs `Trim` on a `time.Ticker` cadence inside
a `select` over `ctx.Done()` — the long-running-loop pattern developed fully in
doc 08.

### 2.9 `Stats` — `make`, `append`, `XLen`, `XRevRangeN`

```go
type DeviceStat struct {
	Device     string
	HotEntries int64
	LastIngest time.Time
}

func (s *Store) Stats(ctx context.Context) ([]DeviceStat, error) {
	devices, err := s.Devices(ctx)
	if err != nil {
		return nil, err
	}
	stats := make([]DeviceStat, 0, len(devices))
	for _, d := range devices {
		n, err := s.rdb.XLen(ctx, streamKey(d)).Result()
		if err != nil {
			return nil, err
		}
		st := DeviceStat{Device: d, HotEntries: n}
		last, err := s.rdb.XRevRangeN(ctx, streamKey(d), "+", "-", 1).Result()
		if err == nil && len(last) == 1 {
			if e, err := decode(last[0]); err == nil {
				st.LastIngest = e.IngestTime.AsTime()
			}
		}
		stats = append(stats, st)
	}
	return stats, nil
}
```

> **Go concept — `make([]T, 0, n)` + `append`.** `make([]DeviceStat, 0,
> len(devices))` creates a slice with **length 0 but capacity `len(devices)`** —
> empty, but with the backing array already sized. Each `append` then fills it
> without a single reallocation, since we know exactly how many stats we'll
> produce. Contrast `make([]T, n)` (length `n`, filled with zero values, which
> you'd then index into). Pre-sizing capacity is a cheap, idiomatic
> optimisation whenever the final count is known up front — the moral cousin of
> `std::vector::reserve`.

`XLen` returns the stream length (entry count) cheaply. `XRevRangeN(..., "+",
"-", 1)` reads the stream **backwards**, one message — i.e. the newest — to
recover its ingest time. Note the deliberately forgiving handling: if the
reverse-range or decode fails, we simply leave `LastIngest` at its zero value
rather than failing the whole stats call.

### 2.10 Consumer-group operations

```go
func (s *Store) EnsureGroup(ctx context.Context, device string) error {
	err := s.rdb.XGroupCreateMkStream(ctx, streamKey(device), Group, "0").Err()
	if err != nil && strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}
```

`EnsureGroup` creates the consumer group (and the stream, `MkStream`, if it
doesn't exist yet) starting at ID `"0"` (the beginning). Redis returns a
`BUSYGROUP` error if the group already exists — which is fine, so we swallow
exactly that one and let anything else through.

```go
func (s *Store) ReadGroup(ctx context.Context, devices []string, consumer string, count int64, block time.Duration) (map[string][]Raw, error) {
	streams := make([]string, 0, len(devices)*2)
	for _, d := range devices {
		streams = append(streams, streamKey(d))
	}
	for range devices {
		streams = append(streams, ">")
	}
	res, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: Group, Consumer: consumer, Streams: streams, Count: count, Block: block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string][]Raw{}
	for _, stream := range res {
		device := strings.TrimPrefix(stream.Stream, "devlog:stream:")
		for _, m := range stream.Messages {
			e, err := decode(m)
			if err != nil {
				_ = s.rdb.XAck(ctx, stream.Stream, Group, m.ID) // corrupt: ack & skip
				continue
			}
			out[device] = append(out[device], Raw{ID: m.ID, Entry: e})
		}
	}
	return out, nil
}
```

> **Go concept — `strings.TrimPrefix` (reversing a key scheme).** `XREADGROUP`
> reports results keyed by the full Redis key (`devlog:stream:dev1`);
> `strings.TrimPrefix(s, "devlog:stream:")` strips the prefix to recover the
> bare device ID for the returned map. If the prefix isn't present it returns the
> string unchanged (no error) — the exact inverse of `streamKey`.

`XReadGroup`'s `Streams` argument is Redis's quirky "N keys then N IDs" format —
hence two loops appending first the keys, then a `">"` per device (`">"` means
"only entries never delivered to this group"). Note again `make([]string, 0,
len(devices)*2)` pre-sizing, and `redis.Nil` here meaning "the blocking read
timed out with nothing new" → `(nil, nil)`.

```go
func (s *Store) Ack(ctx context.Context, device string, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.rdb.XAck(ctx, streamKey(device), Group, ids...).Err()
}
```

> **Go concept — variadic parameters and `...` spreading.** `ids ...string`
> makes `Ack` **variadic**: callers pass any number of IDs (`Ack(ctx, d, a, b)`)
> and inside `ids` is a `[]string`. Passing it onward as `ids...` **spreads** the
> slice back into individual arguments for `XAck`. The empty-slice guard avoids a
> pointless round-trip.

`ClaimStale` (see source) loops `XAutoClaim` to adopt messages another consumer
read but never acked (e.g. it crashed) — see §3.2.

### 2.11 `decode` — the shared unmarshal helper

```go
func decode(m redis.XMessage) (*devicelogv1.LogEntry, error) {
	v, ok := m.Values[entryField].(string)
	if !ok {
		return nil, fmt.Errorf("stream message %s has no entry field", m.ID)
	}
	e := &devicelogv1.LogEntry{}
	if err := proto.Unmarshal([]byte(v), e); err != nil {
		return nil, fmt.Errorf("decode entry %s: %w", m.ID, err)
	}
	return e, nil
}
```

> **Go concept — type assertion with comma-ok.** `m.Values[entryField].(string)`
> is a **type assertion**: `Values` is `map[string]any`, and `.(string)` pulls
> the concrete `string` back out. The two-result form `v, ok := ...` is the safe
> version — `ok` is `false` instead of panicking if the value isn't a string.
> `proto.Unmarshal` (doc 04) then rebuilds the `LogEntry` from those bytes.

---

## 3. Deep dives

### 3.1 Redis Streams as a per-device WAL, with time-encoding IDs

A Redis Stream is an append-only, ordered log: `XADD` appends, `XRANGE` reads a
range, `XLEN` counts, `XTRIMMINID` drops the old tail. devlogd keeps **one
stream per device** for natural isolation (a chatty device can't crowd out
another's reads) and per-device retention.

The pivotal design choice is the **ID scheme**. A stream ID is
`<milliseconds>-<sequence>`, and Redis normally auto-assigns the milliseconds
from *its* clock. devlogd instead supplies them: `strconv.FormatInt(ingestMs,
10) + "-*"`. Now the ID *is* the ingest timestamp. Three operations fall out for
free:

- **Time-range reads** (`Range`) become ID-range reads — `XRANGE` from
  `fromMs` to `toMs` — no secondary index, no scanning-and-filtering.
- **Retention** (`Trim`) becomes `XTRIMMINID minID` where `minID = now −
  hot_retention` in ms — Redis drops everything older in one command.
- **Ordering** is guaranteed: entries sort by ingest time automatically.

Why "WAL"? Because the stream is **durable** and the archiver ([08](08-archive.md))
is a *consumer* of it, not the writer. A producer's entry is safe the instant
`XADD` returns; cold-storage latency or an S3 outage can never back-pressure a
robot in the field. The stream is the write-ahead log; cold storage is the
eventual system of record; `hot_retention` is simply how long the WAL is kept
after the data has been safely drained.

### 3.2 Consumer groups: at-least-once delivery and crash recovery

A plain `XRANGE` read is stateless — Redis doesn't know or care who read what.
That's fine for queries but wrong for archiving, where we must guarantee **every
entry reaches cold storage at least once, even across crashes**. Redis
**consumer groups** provide exactly that, and the four methods map onto its
lifecycle:

- **`EnsureGroup`** (`XGROUP CREATE`) declares a group named `archiver`. The
  group tracks, per stream, the last-delivered ID and a **Pending Entries List
  (PEL)** of messages delivered but not yet acked.
- **`ReadGroup`** (`XREADGROUP ... >`) hands the consumer new messages *and*
  records them in the PEL. Delivery is now "checked out to this consumer,"
  awaiting acknowledgement.
- **`Ack`** (`XACK`) removes an ID from the PEL: "safely archived, forget it."
- **`ClaimStale`** (`XAUTOCLAIM`) is the recovery valve. If a consumer reads
  messages, adds them to the PEL, then *crashes before acking*, those messages
  are stuck. `XAUTOCLAIM` reassigns any message idle longer than `minIdle` to a
  live consumer, which then processes and acks it.

Together these give **at-least-once** semantics: a message stays pending until
someone explicitly acks it, so a crash at any point means the entry is simply
re-delivered later, never dropped. "At least once" (not "exactly once") is why
the pipeline downstream must tolerate duplicates — the query engine (doc 10)
de-duplicates by entry hash. Note also the defensive `decode`-failure handling
in both `ReadGroup` and `ClaimStale`: a genuinely corrupt message is **acked and
skipped**, because leaving it in the PEL forever would wedge the group.

---

## 4. Idioms & gotchas

- **`redis.Nil` is not a failure — usually.** Both `ChainHead` and `ReadGroup`
  translate it to `(nil, nil)`: "no such field" and "blocking read timed out"
  are normal outcomes. Always distinguish `errors.Is(err, redis.Nil)` from a
  real error before treating a call as failed.
- **`.Result()` vs `.Err()`.** Use `.Result()` when you need the value,
  `.Err()` when you only need to know it worked (`Trim`, `Ack`). Forgetting to
  check either means silently ignoring a Redis failure.
- **`string(int)` is a trap.** For number→string always use
  `strconv.FormatInt`, never `string(n)` (which yields the Unicode character for
  that code point). `go vet` flags the common cases, but not all.
- **Pipelines batch; `Exec` executes.** Queued `pipe.X...` calls do nothing
  until `pipe.Exec`. `TxPipeline` adds `MULTI/EXEC` atomicity; a plain
  `Pipeline` only saves round-trips without the all-or-nothing guarantee.
- **Pre-size slices when you know the count.** `make([]T, 0, n)` + `append`
  avoids reallocations — but note it's `0` length, `n` capacity; `make([]T, n)`
  gives you `n` zero-valued elements instead, a different thing.
- **Callbacks that can stop.** A `func(...) bool` streaming callback keeps memory
  flat and lets the caller break early — prefer it over returning an unbounded
  slice.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why does `AppendWithChain` use `TxPipeline` rather than three
   separate calls? What could a reader observe if the `XADD` and `HSET` were
   *not* atomic?
2. **Recall.** In `ChainHead`, what does `errors.Is(err, redis.Nil)` catch, and
   why is returning `(nil, nil)` correct rather than an error?
3. **Apply.** Add a `Count(ctx, device) (int64, error)` method using `XLen`.
   Which result-wrapper method do you call, and how do you propagate a Redis
   error?
4. **Apply.** `Range` re-fetches in pages of 1000. Add a `limit int` parameter
   that stops after emitting `limit` entries. (Hint: you already have the
   early-stop mechanism — count inside the caller's `fn`, or add a counter in
   `Range`.)
5. **Extend.** `Stats` swallows errors from `XRevRangeN`/`decode` to keep
   `LastIngest` best-effort. Write a table-driven test (peek at
   `store_test.go`, which uses `miniredis`) proving a device with a corrupt last
   message still reports a correct `HotEntries` count.
6. **Hero.** Trace an entry's full at-least-once path: `AppendWithChain` →
   `ReadGroup` → (simulated crash before `Ack`) → `ClaimStale` on another
   consumer → `Ack`. Explain at which points a crash re-delivers vs. drops the
   entry, and why the downstream must de-duplicate.

---

## 6. Recap & next

You've now driven a real third-party client end to end: holding a
`*redis.Client`, unpacking its **typed result wrappers**, and reading the
**sentinel** `redis.Nil` with `errors.Is`. You wrapped writes in a
**transaction** (`TxPipeline`/`Exec`), built stream IDs with `strconv`, streamed
results through a **higher-order callback** with early-stop, pre-sized slices
with `make`/`append`, and used `sort.Strings` and `strings.TrimPrefix`. On the
domain side you saw why a Redis Stream makes an ideal per-device **WAL**, how
time-encoding IDs make range reads and retention nearly free, and how **consumer
groups** deliver at-least-once with crash recovery.

**Next:** [07 — cold](07-cold.md), where the entries drained from these streams
are encoded into compressed, checksummed segments — and you meet
`io.Reader`/`io.Writer`, `bufio`, varint encoding, and `defer`.
