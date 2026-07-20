# Component 09 — `internal/ingest`

> **Role:** turn one authenticated MQTT payload into a signed, hash-chained,
> stored `LogEntry`, and fan freshly ingested entries out to live gRPC `Tail`
> subscribers. | **Source:** `internal/ingest/pipeline.go`,
> `internal/ingest/hub.go`

**Where this sits in the journey:** this is the **concurrency** doc. Until now
every package you read ran on one call stack at a time. `ingest` is called from
*many* goroutines at once — one per connected device — and it broadcasts to
*many* more — one per live tail. So this is where Go's concurrency toolkit earns
its reputation: **goroutines**, **`sync.Mutex`/`sync.RWMutex`**, **channels**,
and **`select`**. Prerequisites: [02 — config](02-config.md) (structs, methods,
maps, pointers, errors), [04 — sign](04-sign.md) (`ChainSign`, the hash chain),
and [06 — hot](06-hot.md) (`hot.Store`, `ChainHead`, `AppendWithChain`). We lean
on those for the domain and go deep on everything concurrent, which is new.

## What you'll master

- **Go:** **goroutines** and the M:N scheduler; **`sync.Mutex`** guarding a
  critical section; **`sync.RWMutex`** for read-mostly state; **channels**
  (`chan T`, **buffered** channels, send `ch <- v`, receive `<-ch`, `close`, and
  closed-channel receive semantics); **directional channel types** (`<-chan T`);
  **`select` with a `default`** for a non-blocking send; `defer mu.Unlock()`
  **vs** manual unlock, and why this code does both; **maps as registries**;
  passing a `*LogEntry` pointer to share (not copy) one record.
- **Domain:** server-owned field enforcement, **ULID** entry ids, the per-device
  hash-chain critical section, an in-memory chain-head cache that stays correct
  on failure, and a best-effort live-tail hub with a deliberate drop policy.

---

## 1. Orientation

`ingest.Pipeline.Ingest` is the write plane's beating heart. The broker (doc 11)
has already authenticated the device and handed us the raw bytes; from here the
pipeline must: decode the protobuf, **overwrite the fields the server owns** so a
lying producer can't forge them, link the entry to the device's previous entry
(the tamper-evident chain from doc 04), persist it durably (doc 06), and only
*then* nudge any live listeners.

Two invariants make this hard. First, the chain must be **linear per device**:
entry N's `prev_hash` must be the hash of entry N-1, which means reading the head,
signing, and appending have to happen as *one indivisible step* even though many
devices' messages arrive concurrently. Second, a live tail that falls behind must
**never** slow ingestion down — durable storage is the source of truth, the tail
is a convenience. `pipeline.go` solves the first with a mutex; `hub.go` solves the
second with buffered channels and a non-blocking send.

---

## 2. Guided code walkthrough

### 2.1 The package doc comment

```go
// Package ingest turns authenticated MQTT payloads into signed, chained,
// stored log entries, and fans live entries out to gRPC Tail subscribers.
package ingest
```

Same package-comment convention as doc 02. Note that two files (`pipeline.go`,
`hub.go`) share `package ingest`: they are one compilation unit and freely see
each other's unexported names (`Hub`, `chains`), with no headers.

### 2.2 The `Pipeline` struct — state that many goroutines touch

```go
type Pipeline struct {
	hot     *hot.Store
	signer  *sign.Signer
	hub     *Hub
	metrics *telemetry.Metrics
	log     *slog.Logger

	// mu serializes hash-chain updates: each entry must link to the true
	// previous entry, so chain-read → sign → append is one critical section.
	mu     sync.Mutex
	chains map[string][]byte
}
```

The top five fields are injected dependencies (doc 03's DI pattern). The last two
are the interesting part: `mu` protects `chains`, an in-memory cache mapping each
`device_id` to the hash of its most recently stored entry.

> **Go concept — goroutines.** A **goroutine** is a function running
> concurrently, started with `go f(args)`. It is *not* an OS thread: goroutines
> are multiplexed **M:N** onto a small pool of OS threads by the Go runtime
> scheduler, so they cost ~2 KB of stack to start and you can have hundreds of
> thousands live. Compare a C++ `std::thread`, which is a 1:1 wrapper over a
> heavyweight kernel thread. You won't see `go` *in these two files* — but the
> broker starts a goroutine per MQTT connection and the gRPC server starts one
> per `Tail` RPC, and **all of those call into this `Pipeline` and `Hub`
> simultaneously**. That is exactly why the struct carries a mutex: shared
> mutable state (`chains`, `subs`) touched by concurrent goroutines is a data
> race unless it is synchronised.

> **Go concept — `sync.Mutex` as a struct field.** `sync.Mutex` is a mutual-
> exclusion lock. Declaring it as a plain field (not a pointer) is idiomatic: the
> zero value of a `Mutex` is a ready-to-use unlocked lock — no constructor, no
> `pthread_mutex_init`. Convention is to place `mu` *immediately above the fields
> it guards*, so a reader knows its scope at a glance. **Never copy a `Mutex`
> after first use** (copying a `sync.Mutex` copies its internal state and breaks
> it), which is one more reason `Pipeline` is always passed as `*Pipeline`.

### 2.3 The constructor

```go
func NewPipeline(h *hot.Store, s *sign.Signer, hub *Hub, m *telemetry.Metrics, log *slog.Logger) *Pipeline {
	return &Pipeline{hot: h, signer: s, hub: hub, metrics: m, log: log, chains: map[string][]byte{}}
}
```

Nothing new since doc 02 — dependency injection, a struct literal, a returned
pointer. The one thing to notice: `chains` is initialised to an empty map
(`map[string][]byte{}`). A `nil` map is readable but **panics on write**, and
this map is written on every ingest, so it must be made non-nil here.

### 2.4 `Ingest` — decode and enforce server-owned fields

```go
func (p *Pipeline) Ingest(ctx context.Context, deviceID, subsystem string, payload []byte) error {
	e := &devicelogv1.LogEntry{}
	if err := proto.Unmarshal(payload, e); err != nil {
		p.metrics.IngestErrors.WithLabelValues("decode").Inc()
		return fmt.Errorf("payload is not a LogEntry: %w", err)
	}
	// The authenticated MQTT identity is authoritative.
	if e.DeviceId == "" {
		e.DeviceId = deviceID
	} else if e.DeviceId != deviceID {
		p.metrics.IngestErrors.WithLabelValues("identity_mismatch").Inc()
		return fmt.Errorf("entry claims device %q but session is %q", e.DeviceId, deviceID)
	}
	if e.Subsystem == "" {
		e.Subsystem = subsystem
	}
	e.EntryId = ulid.Make().String()
	e.IngestTime = timestamppb.Now()
	if e.DeviceTime == nil {
		e.DeviceTime = e.IngestTime
	}
	e.Audit = nil // server-owned; ignore whatever the producer sent
	// …
```

`e := &devicelogv1.LogEntry{}` allocates a zero-valued entry and we decode the
wire bytes into it. Then we **impose the server's will**: the `device_id` is
taken from (or checked against) the *authenticated session*, never trusted from
the payload; a mismatch is a hard error. `subsystem` defaults from the topic.
Crucially we **stamp `EntryId` and `IngestTime` ourselves and null out `Audit`** —
the pipeline is the only authority for those, so a malicious producer that pre-
fills them is simply overwritten (the `TestIngestOverwritesProducerAudit` test
pins this behaviour).

> **Go concept — ULIDs vs UUIDs.** `ulid.Make().String()` mints a **ULID** — a
> 128-bit id whose high 48 bits are a millisecond timestamp and whose low 80 bits
> are randomness. Rendered as 26 Crockford-base32 characters, ULIDs are
> **lexicographically sortable in time order**: sort the *strings* and you get
> chronological order, for free. A random UUIDv4 has no such property — two ids
> minted a second apart sort arbitrarily. That ordering is why the query engine
> (doc 10) can dedupe and tie-break by `entry_id` cheaply, and why the id doubles
> as a coarse timestamp. `ulid.Make()` returns a value; `.String()` renders it.

> **Go concept — `timestamppb.Now()` and well-known types.** protobuf has a
> canonical `google.protobuf.Timestamp` message; its Go binding lives in
> `types/known/timestamppb`. `timestamppb.Now()` returns a `*timestamppb.Timestamp`
> for the current instant — the wire-friendly equivalent of `time.Now()`. We use
> it because `LogEntry.IngestTime` is a proto field, not a Go `time.Time`; the two
> convert with `.AsTime()` / `timestamppb.New(t)`. `DeviceTime` defaulting to
> `IngestTime` when the producer omitted it keeps every entry sortable even from a
> clockless device.

> **Go concept — a pointer *is* the record.** `e` is a `*LogEntry`. Every mutation
> above (`e.DeviceId = …`, `e.Audit = nil`) writes through that pointer to one
> heap object. When we later pass `e` to `ChainSign`, `AppendWithChain`, and
> `hub.Publish`, they all receive the **same pointer** — no copy, and the audit
> block `ChainSign` fills in is visible to everyone afterwards. This is the Go
> habit of threading one `*Struct` through a pipeline rather than passing values
> around. (It also means: once `e` is shared with the hub, we must not keep
> mutating it — and we don't.)

### 2.5 The critical section — read head, sign, append, atomically

```go
	start := time.Now()
	p.mu.Lock()
	prev, ok := p.chains[e.DeviceId]
	if !ok {
		var err error
		if prev, err = p.hot.ChainHead(ctx, e.DeviceId); err != nil {
			p.mu.Unlock()
			p.metrics.IngestErrors.WithLabelValues("store").Inc()
			return err
		}
	}
	if err := sign.ChainSign(e, prev, p.signer); err != nil {
		p.mu.Unlock()
		p.metrics.IngestErrors.WithLabelValues("sign").Inc()
		return err
	}
	if err := p.hot.AppendWithChain(ctx, e); err != nil {
		// Not advancing the in-memory chain keeps the next entry linked to
		// the last durably stored one.
		delete(p.chains, e.DeviceId)
		p.mu.Unlock()
		p.metrics.IngestErrors.WithLabelValues("store").Inc()
		return err
	}
	p.chains[e.DeviceId] = e.Audit.EntryHash
	p.mu.Unlock()
```

This is the heart of the doc. Between `Lock` and `Unlock` we do three dependent
steps that **must not interleave** with another goroutine's ingest for the same
device: read the previous hash, sign this entry onto it, and append.

> **Go concept — a critical section.** `p.mu.Lock()` blocks until this goroutine
> holds the lock; `p.mu.Unlock()` releases it. Everything between is the
> **critical section** — at most one goroutine runs it at a time. Why is it
> needed here? Imagine two messages from `dev1` arriving on two goroutines. Both
> read `prev = hashA`. Both sign onto `hashA`. Now two entries claim the same
> predecessor — the chain **forks**, and doc 10's `VerifyRange` would flag it.
> The lock forces the sequence to be **read → sign → append → publish head**,
> serialised, so the second message reads the *first's* new hash. This is the
> classic read-modify-write race, and the mutex is the standard fix — identical
> in intent to a C++ `std::mutex` + `std::lock_guard`.

> **Go concept — the chain-head cache and the comma-ok read.** `prev, ok :=
> p.chains[e.DeviceId]` is a **map read with the comma-ok idiom** (doc 02):
> `ok` is `false` on a cold cache. On a miss we pay one Redis round-trip
> (`ChainHead`) to learn the durable head; on a hit we skip it. On success we
> write the new head back: `p.chains[e.DeviceId] = e.Audit.EntryHash`. So the map
> is an **in-memory cache of per-device chain heads** — steady state does zero
> extra reads.

> **Go concept — correctness-preserving cache invalidation.** Look at the
> `AppendWithChain` failure branch: it calls `delete(p.chains, e.DeviceId)`.
> This is the subtle, important line. If the store write failed we do **not**
> know whether the entry landed, so we must not advance the cached head. Deleting
> the entry forces the *next* ingest to re-read the true durable head from Redis,
> re-synchronising the cache with reality. A cache that lies about the head would
> chain future entries onto a hash that isn't actually stored — a silent, later-
> discovered corruption. `delete(m, k)` is Go's built-in map delete; deleting an
> absent key is a safe no-op.

> **Go concept — `defer mu.Unlock()` vs manual unlock (and why this code goes
> manual).** The textbook Go pattern is `mu.Lock(); defer mu.Unlock()` — the
> `defer` runs on *every* return path, so you can never forget to unlock (you'll
> see exactly this in the Hub below). This function **deliberately unlocks
> manually** instead, releasing the lock the instant the protected work is done.
> Why? Because after the critical section it still does `hub.Publish` and
> `observe` — work that does **not** touch `chains` and therefore should **not**
> hold the per-device chain lock, which would needlessly serialise the broadcast.
> The cost is discipline: every early-return inside the section repeats
> `p.mu.Unlock()` before `return`. That is the trade you make when you want the
> lock held for the *minimum* span rather than the whole function body. (A common
> hybrid: wrap just the critical section in a small closure with its own
> `defer`.)

### 2.6 After the lock — publish and observe

```go
	p.hub.Publish(e)
	p.observe(e, len(payload), time.Since(start))
	return nil
}
```

With the entry durably stored and the lock released, we hand the *same*
`*LogEntry` to the hub for live tails and record metrics (`observe` bumps latency
histograms and per-severity counters — routine Prometheus work from doc 03). If
no one is tailing, `Publish` is nearly free.

### 2.7 The `Hub` struct — a registry of subscriber channels

```go
type Hub struct {
	mu   sync.RWMutex
	next int
	subs map[int]chan *devicelogv1.LogEntry
}

func NewHub() *Hub {
	return &Hub{subs: map[int]chan *devicelogv1.LogEntry{}}
}
```

The hub is an in-process publish/subscribe broadcaster. `subs` maps a
subscription id to that subscriber's delivery channel; `next` is a monotonic id
counter.

> **Go concept — maps as registries.** You met maps as lookup tables in doc 02;
> here a map is a **registry of live things**. `map[int]chan *LogEntry` associates
> an integer handle with a *channel*. `Subscribe` inserts, `Unsubscribe` deletes,
> `Publish` ranges over the values. This "map from opaque id to resource" is the
> Go idiom for a dynamic set of subscribers/sessions/connections — the same shape
> as `Verifier`'s `map[keyID]key` (doc 04) but with a channel as the value.

> **Go concept — `sync.RWMutex`.** A `RWMutex` has two modes: `Lock`/`Unlock`
> for **writers** (exclusive) and `RLock`/`RUnlock` for **readers** (shared —
> many readers at once, but none while a writer holds it). Use it when the data
> is **read far more often than written**. Here `Publish` (the hot path, called on
> every ingest) only *reads* the `subs` map, so it takes `RLock` and multiple
> concurrent publishes never block each other; `Subscribe`/`Unsubscribe` (rare)
> take the exclusive `Lock`. A plain `Mutex` would be correct but would serialise
> every publish. C++'s equivalent is `std::shared_mutex` with `shared_lock` /
> `unique_lock`.

### 2.8 `Subscribe` — hand out a buffered, receive-only channel

```go
func (h *Hub) Subscribe(buffer int) (int, <-chan *devicelogv1.LogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan *devicelogv1.LogEntry, buffer)
	h.subs[id] = ch
	return id, ch
}
```

A gRPC `Tail` handler calls this to join the broadcast, then loops receiving from
the returned channel until its client disconnects.

> **Go concept — channels.** A **channel** is a typed, thread-safe conduit
> between goroutines: `chan *LogEntry` carries pointers to entries. `make(chan T,
> n)` creates one; you **send** with `ch <- v` and **receive** with `v := <-ch`.
> Sends and receives synchronise the goroutines — this is Go's "share memory by
> communicating" model, replacing the condition-variable + queue you'd hand-roll
> in C++. A channel value is a reference (like a map): copying it copies the
> handle, not the buffer.

> **Go concept — buffered channels.** `make(chan T, buffer)` gives the channel an
> internal queue of `buffer` slots. A send succeeds immediately while the buffer
> has room and only blocks when it's **full**; a receive blocks only when it's
> **empty**. An *unbuffered* channel (`make(chan T)`) has zero slots — every send
> blocks until a receiver is ready (a rendezvous). The buffer here is the
> subscriber's tolerance for bursts: a tail that briefly lags can have up to
> `buffer` entries queued before the hub's drop policy kicks in (§2.10).

> **Go concept — directional channel types.** The return type is
> `<-chan *LogEntry`, a **receive-only** channel. The hub keeps the bidirectional
> `chan` (so *it* can send); the subscriber gets a view it can only receive from.
> The compiler enforces this: a subscriber that tried `ch <- x` wouldn't compile.
> Directional types document intent and prevent a consumer from accidentally
> sending or closing a channel it doesn't own. (`chan<- T` is the send-only dual.)

> **Go concept — `defer` for lock release (the common case).** Unlike the
> pipeline, `Subscribe`/`Unsubscribe`/`Publish` use `defer h.mu.Unlock()` /
> `RUnlock()`. These functions are short, do nothing after the guarded work, and
> have multiple exit points, so `defer` is the safe, idiomatic choice: it runs
> the unlock on return no matter how the function exits.

### 2.9 `Unsubscribe` — delete and close

```go
func (h *Hub) Unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}
```

> **Go concept — `close(ch)` and closed-channel receive semantics.** `close(ch)`
> marks a channel done. A receiver draining a closed channel gets every buffered
> value first, then an endless stream of **zero values** with the two-value form
> reporting *not-ok*: `v, ok := <-ch` yields `ok == false` once the channel is
> closed and drained. This is how a `Tail` loop learns to stop — `for e := range
> ch` exits cleanly when the hub closes the channel. Two rules matter: **only the
> sender should close** (the hub owns the channel, so the hub closes it — here on
> unsubscribe), and **sending on a closed channel panics**. Ordering the guarded
> code as `delete` *then* `close`, all under the write lock, guarantees no
> `Publish` can pick this channel out of the map and send to it after it's closed.

### 2.10 `Publish` — the non-blocking broadcast

```go
func (h *Hub) Publish(e *devicelogv1.LogEntry) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs {
		select {
		case ch <- e:
		default: // subscriber too slow — drop
		}
	}
}
```

For each subscriber we *try* to send the entry — but never wait.

> **Go concept — `select` with a `default` (non-blocking send).** `select` is
> like a `switch` whose cases are channel operations; it runs whichever is ready.
> The magic is the `default` case: **if no channel op is immediately ready,
> `select` runs `default` instead of blocking.** So `case ch <- e:` sends only if
> the subscriber's buffer has room; otherwise control falls to the empty
> `default` and we simply **move on, dropping the entry** for that one slow
> subscriber. Contrast a plain `ch <- e` with no `select`: that send would
> **block** until the subscriber made room — and because we're holding the read
> lock and iterating every subscriber, one stalled tail would freeze the whole
> broadcast and, through back-pressure, every ingesting device. The `default` is
> what turns "reliable delivery" into "best-effort delivery," which is exactly the
> policy the hub wants (see §3.2).

---

## 3. Deep dives

### 3.1 Why exactly one mutex serialises ingest, and how failure keeps the chain linear

The hash chain (doc 04) is only tamper-*evident* if it is **linear per device**:
a single path where each entry names its true predecessor. Producing that under
concurrency is a textbook read-modify-write problem. The "value" being modified
is the device's chain head; the three steps — **read** the head, **sign** the new
entry onto it, **append** and publish the new head — must be atomic *with respect
to other ingests of the same device*.

`p.mu` provides that atomicity. Note what it does and doesn't cover:

- It is **one mutex for the whole pipeline**, not one per device. That is a
  simplicity-over-throughput choice: two *different* devices briefly contend even
  though their chains are independent. At fleet-log rates the critical section is
  microseconds (a map read, an Ed25519 sign, a Redis pipeline), so a single lock
  is plenty; sharding to a `map[device]*sync.Mutex` is a documented future option
  if a hot device ever dominates.
- The lock is **released before `hub.Publish` and `observe`** (§2.5). Those don't
  touch `chains`, so holding the chain lock across them would only add contention.
  This is why the code unlocks manually rather than with a single `defer`.

The failure handling is what keeps the chain honest. There are three exits inside
the section — `ChainHead` error, `ChainSign` error, `AppendWithChain` error —
and each unlocks before returning. The `AppendWithChain` case additionally does
`delete(p.chains, e.DeviceId)`. Walk the states:

- **Append succeeds** → cache advances to `e.Audit.EntryHash`; next entry chains
  onto it with no Redis read. Correct and fast.
- **Append fails** → we cannot know if the write landed, so we **evict** the
  cached head. The next ingest sees a cache miss and re-reads the durable head
  from Redis via `ChainHead`, re-synchronising with what's actually stored.

The invariant preserved is: *the cached head is only ever advanced to a hash we
know is durably stored.* Anything less would chain later entries onto a phantom,
and doc 10's verifier would eventually report a break with no attacker in sight.
Note too that the fresh entry is only shared with the hub *after* a successful
append — a dropped/failed entry never reaches a live tail, so tails never show an
entry that isn't in storage.

### 3.2 Best-effort fan-out: why a slow tail must never stall ingest

The Hub embodies one deliberate decision: **liveness of the write plane
outranks completeness of a live tail.** Storage (Redis, then the cold bucket) is
the authoritative history; `Tail` is a real-time convenience for an operator
watching a robot right now. If an operator's connection, laptop, or network
hiccups, their tail channel's buffer fills. The question is what happens to
ingestion at that moment.

With a blocking send (`ch <- e`), the answer is catastrophic: `Publish` would
wait for that one slow reader, holding the read lock, inside the ingest path,
which is itself driven by the MQTT broker — so a single stalled operator would
apply back-pressure all the way to *every device in the field*. A logging system
that goes silent because someone's `tail` window froze is unacceptable.

The `select { case ch <- e: default: }` inverts the priority. Each subscriber
gets a bounded buffer (its burst tolerance) and beyond that, entries for **that
subscriber only** are dropped. Ingestion never waits. The dropped entries aren't
lost — they're in Redis and will appear in a `Query`; only the *live* view
skips them, and only for the lagging client. This is why the package doc calls
delivery "best-effort" and why the failure table in `COMPONENTS.md` §15 scopes a
slow tail's impact to "that stream only; history intact." The whole mechanism is
three lines, but it encodes a real availability decision.

The pairing of `RWMutex` + per-subscriber buffered channel + non-blocking send is
a compact, idiomatic Go broadcaster. Each piece pulls its weight: `RLock` lets
concurrent publishes proceed; the buffer absorbs jitter; the `default` guarantees
forward progress.

---

## 4. Idioms & gotchas

- **Put `mu` above what it guards, and pass by pointer.** The comment on
  `Pipeline.mu` names its exact scope. Never copy a struct containing a live
  `Mutex`/`RWMutex` — always use `*Pipeline`, `*Hub`.
- **Hold the lock for the minimum span.** The pipeline unlocks the instant the
  chain work is done and does `Publish`/`observe` lock-free. Manual unlock buys
  that at the price of repeating `Unlock()` on every early return — get one wrong
  and you deadlock the next ingest.
- **A `nil` map reads but panics on write.** `chains` and `subs` are initialised
  in their constructors precisely because both are written.
- **Only the sender closes a channel; a send on a closed channel panics.** The
  hub owns each channel and closes it in `Unsubscribe`, under the write lock,
  after removing it from the map — so no `Publish` can send to a closed channel.
- **`default` in `select` = non-blocking.** Its presence flips a channel op from
  "wait until ready" to "do it if you can, else fall through." Omitting it here
  would silently convert best-effort delivery into a system-wide stall.
- **Server owns identity, ids, time, and audit.** Producer-supplied `device_id`
  mismatches are rejected; `entry_id`, `ingest_time`, and `audit` are overwritten
  unconditionally. Trust the session, not the payload.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why does `Ingest` unlock `p.mu` *manually* before `hub.Publish`,
   while the Hub methods use `defer`? What would holding the lock across
   `Publish` cost?
2. **Recall.** In the `AppendWithChain` failure branch, why `delete(p.chains,
   e.DeviceId)` rather than leaving the old head in place or advancing it?
3. **Apply.** Change `Publish`'s `select` to a plain blocking send `ch <- e`.
   Describe the exact chain of events when one tail subscriber stops receiving.
   Which goroutines block, and what does a field device observe?
4. **Apply.** The single pipeline mutex briefly serialises unrelated devices.
   Sketch a `map[string]*sync.Mutex` per-device design. What new problem does the
   *map itself* introduce, and which lock now guards it?
5. **Extend.** Add a `Hub.SubscriberCount() int` that reports how many tails are
   live. Which lock (`RLock` or `Lock`) is correct, and why?
6. **Hero.** Add a dropped-entries counter to the hub so `default` increments a
   Prometheus metric per subscriber. Where must the increment go to stay race-
   free, and how would you expose per-subscriber drop rates without holding the
   lock while touching Prometheus?

---

## 6. Recap & next

You now have Go's concurrency core in your hands, learned on real code: goroutines
and the M:N scheduler; a `sync.Mutex` guarding a read-modify-write **critical
section** so a per-device hash chain stays linear; a `sync.RWMutex` for a
read-mostly registry; **channels** — buffered, directional, closed — as the
conduit between producer and consumer goroutines; and `select` with `default` as
the non-blocking send that encodes a real availability policy. You also saw two
signature designs: the one-mutex ingest critical section with cache eviction on
failure, and the best-effort fan-out hub that refuses to let a slow tail stall the
write plane.

**Next:** [10 — query](10-query.md), where those stored, chained entries are read
back — merging the hot and cold tiers, deduplicating by ULID, sorting, and
verifying the chain end to end.
