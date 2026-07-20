# Component 08 — `internal/archive`

> **Role:** a background worker that continuously drains the hot WAL (Redis
> streams) into cold segments — batching by size and time, acking only after a
> segment is durable, and draining cleanly on shutdown. | **Source:**
> `internal/archive/archiver.go`

**Where this sits in the journey:** this is your first **long-running
concurrent worker**. Everything before it was request/response or one-shot
(`config.Load`, `sign`, a single query). The archiver instead *lives* — it owns
a `for {}` loop that runs for the life of the process until someone cancels it.
That single shift brings in the whole Go concurrency toolkit: `context` for
cancellation, timers for the sleep-or-quit choice, and mutable state carried
across loop turns. Prerequisites: [02 — config](02-config.md) (structs,
methods, receivers, errors), [06 — hot](06-hot.md) (the Redis `Store`, consumer
groups, `XACK`/`XAUTOCLAIM`), and [07 — cold](07-cold.md) (the `Writer` that
turns a batch into a durable segment). This doc wires those two tiers together.

## What you'll master

- **Go:** the **long-running event loop** (`for {}` driven by `Run(ctx)`);
  **`context.Context`** as the cancellation signal — checking `ctx.Err()` and
  why context is always the *first* argument; **timers** (`time.NewTimer`) and
  the **cancel-or-sleep** `select` pattern; a **stateful struct** carrying a
  mutable `batch`/`acks`/counters across iterations; **maps of slices**
  (`map[string][]string`); `proto.Size`; **backoff on error**; using a **fresh
  `context.WithTimeout`** during shutdown; and a fresh look at **pointer
  receivers** now that mutation actually matters.
- **Domain:** draining a hot WAL into cold segments; batching by size/time;
  **at-least-once + ack-after-durable**; **bounded-memory backpressure** during
  an outage; **crash recovery** via reclaim; **graceful drain** on shutdown; and
  exactly-once *effect* through downstream dedup.

---

## 1. Orientation

The hot tier (doc 06) is a fast, bounded write-ahead log in Redis: every device
appends signed entries to a stream. But Redis is memory; it can't hold history
forever. The cold tier (doc 07) is the opposite: cheap, durable object storage,
written in compressed multi-entry *segments*. The archiver is the pump between
them. It reads new entries from every device's stream through a shared **consumer
group**, accumulates them into a `batch`, and when the batch is big enough or old
enough, writes it as one cold segment. Only *after* that segment is durably in
the bucket does it acknowledge (`XACK`) the entries back to Redis, so they can be
trimmed.

That ordering — **write the segment, then ack** — is the whole safety story. If
the process crashes between writing and acking, the entries are simply un-acked,
so the next run re-reads and re-archives them. Cold storage therefore receives
each entry *at least once*; the query layer (doc 10) deduplicates by entry ID, so
the *observable* result is exactly-once. This is the classic way to get
exactly-once *effect* out of at-least-once *delivery* without a distributed
transaction.

---

## 2. Guided code walkthrough

### 2.1 The package doc comment

```go
// Package archive drains the hot tier into cold segments through a Redis
// consumer group: entries are acked only after the segment is durably in the
// bucket, so delivery to cold storage is at-least-once (queries deduplicate
// by entry ID).
package archive
```

The doc comment states the contract up front (you met package doc comments in
doc 02). Read it as a promise: *acked only after durable ⇒ at-least-once*. Every
design decision below serves that one sentence.

### 2.2 The stateful struct

```go
type Archiver struct {
	hot      *hot.Store
	writer   *cold.Writer
	interval time.Duration
	maxBytes int
	maxCount int
	metrics  *telemetry.Metrics
	log      *slog.Logger
	consumer string

	batch      []*devicelogv1.LogEntry
	batchBytes int
	acks       map[string][]string // device → stream ids awaiting ack
	lastFlush  time.Time
}
```

The top block is **dependencies** (injected once, never mutated): the two tier
stores, the flush thresholds, metrics, logger, and this worker's `consumer`
name. The bottom block, after the blank line, is **mutable working state** that
evolves as the loop runs: the growing `batch`, its running byte count, the
per-device `acks` still owed to Redis, and when we last flushed.

> **Go concept — a struct as a mini state machine.** In C++ you'd reach for a
> class with private members and a `run()` method; a Go struct with methods is
> the same shape minus the access keywords (visibility is by capitalization —
> doc 02). What's worth noticing is *what* lives here: instead of local
> variables inside the loop, the batch and counters are **fields**. That makes
> them survive across loop iterations *and* be reachable from other methods
> (`flush`, `finalFlush`, `reclaim`) without threading them through every call.
> The struct *is* the worker's memory.

> **Go concept — maps of slices.** `acks map[string][]string` maps a device ID
> to a *slice* of Redis stream IDs awaiting ack. Reading a missing key yields the
> value's zero (a `nil` slice), but you cannot *write* to a nil map — so it's
> initialized in `New` (below). The elegant part is appending to a missing key:
> `a.acks[d] = append(a.acks[d], id)` works even when `a.acks[d]` doesn't exist
> yet, because `append(nil, x)` allocates a fresh slice. This is Go's
> `map<string, vector<string>>`, but with no ceremony to grow the bucket.

### 2.3 The constructor

```go
func New(h *hot.Store, w *cold.Writer, interval time.Duration, maxBytes, maxCount int,
	metrics *telemetry.Metrics, log *slog.Logger) *Archiver {
	return &Archiver{
		hot: h, writer: w,
		interval: interval, maxBytes: maxBytes, maxCount: maxCount,
		metrics: metrics, log: log,
		consumer: "archiver-1",
		acks:     map[string][]string{},
	}
}
```

Dependency injection, exactly as in doc 03: everything the worker needs is
handed in, nothing is reached for globally. Note `acks` is initialized to an
empty (non-nil) map so the very first `append`-into-key works, and `consumer` is
seeded with a stable name — the consumer group uses it to remember which
messages *this* worker has read but not yet acked (crucial for `reclaim`, §2.7).

> **Go concept — value vs pointer receiver, revisited.** In doc 02 the
> distinction was mostly academic (one tiny mutation). Here it's the backbone.
> Every method below is declared `func (a *Archiver) …` — a **pointer receiver**
> — because they all mutate the worker's state: `flush` clears `a.batch`, the
> loop appends to it, `reclaim` fills it. A *value* receiver would operate on a
> **copy** of the struct; your carefully accumulated batch would vanish when the
> method returned. The rule from doc 02 ("pointer receiver if you mutate")
> stops being a style tip and becomes correctness.

### 2.4 `Run` — the long-running event loop

```go
func (a *Archiver) Run(ctx context.Context) error {
	a.reclaim(ctx)
	a.lastFlush = time.Now()
	for {
		if ctx.Err() != nil {
			a.finalFlush()
			return nil
		}
		devices, err := a.hot.Devices(ctx)
		if err != nil {
			a.pause(ctx, err)
			continue
		}
		if len(devices) == 0 {
			a.sleep(ctx, time.Second)
			continue
		}
		// … ensure a consumer group exists for each device …
```

> **Go concept — the `for {}` event loop.** `for {}` with no condition is Go's
> infinite loop (there is no `while`; `for` is the only loop keyword). This is
> the heart of a long-running worker: it does one *turn* of work — check for
> shutdown, list devices, read a batch, maybe flush — then loops. Unlike a C++
> thread that you might block on a condition variable, a Go worker like this
> stays a plain function; the runtime will multiplex it onto an OS thread as a
> **goroutine** (you'll launch it as one in doc 14 via `errgroup`). The loop
> never returns on its own — only the `ctx.Err()` check breaks it.

> **Go concept — `context.Context` and why it's the first argument.** `ctx` is
> the **cancellation and deadline signal** that flows down through every blocking
> call. By strong convention it is always the *first* parameter of any function
> that does I/O or can block (`Run(ctx)`, `a.hot.Devices(ctx)`,
> `a.writer.Write(ctx, …)`). Passing it explicitly — rather than stashing it in
> the struct — makes the cancellation scope visible at every call site and lets a
> caller pass a *different* context per call (as `finalFlush` does). It's an
> ambient "should I still be doing this?" token that C++ has no built-in
> equivalent for; you'd otherwise thread a `std::stop_token` everywhere.

> **Go concept — `ctx.Err()`: polling for cancellation.** `ctx.Err()` returns
> `nil` while the context is live and a non-nil error (`context.Canceled` or
> `context.DeadlineExceeded`) once it's done. Checking it at the *top* of each
> turn asks, before starting fresh work, "were we told to stop?" — if so, drain
> and return. This is **cooperative cancellation**: Go never force-kills a
> goroutine, so a worker that ignores its context runs forever.

> **Go concept — `continue` as flow control in a state loop.** Each guard ends
> in `continue`, jumping straight back to the top of the loop. On an error we
> `pause` (back off) and retry the whole turn; with no devices we `sleep` a
> second and retry. This keeps the happy path un-indented at the bottom of the
> loop body rather than nested inside `if`s — the same readability instinct as
> early `return … err` in doc 02, applied to a loop.

### 2.5 Reading, accumulating, and the flush decision

```go
		// Batch already full (previous flush failed): retry the flush with
		// backoff instead of reading more — keeps memory bounded during a
		// bucket outage while the backlog waits safely in Redis.
		if len(a.batch) >= a.maxCount || a.batchBytes >= a.maxBytes {
			a.flush(ctx)
			if len(a.batch) > 0 {
				a.sleep(ctx, time.Second)
			}
			continue
		}
		msgs, err := a.hot.ReadGroup(ctx, devices, a.consumer, int64(a.maxCount-len(a.batch)), 2*time.Second)
		if err != nil && ctx.Err() == nil {
			a.pause(ctx, err)
			continue
		}
		for device, raws := range msgs {
			for _, raw := range raws {
				a.batch = append(a.batch, raw.Entry)
				a.batchBytes += proto.Size(raw.Entry)
				a.acks[device] = append(a.acks[device], raw.ID)
			}
		}
		full := len(a.batch) >= a.maxCount || a.batchBytes >= a.maxBytes
		due := time.Since(a.lastFlush) >= a.interval && len(a.batch) > 0
		if full || due {
			a.flush(ctx)
		}
	}
}
```

Two things happen here. First, the **backpressure guard**: if the batch is
already full, a *previous* flush must have failed (a healthy flush empties the
batch). Rather than read *more* — which would grow memory unboundedly during a
bucket outage — the worker just retries the flush, sleeps a beat if it still
fails, and loops. The unread backlog waits safely in Redis, where it's already
durable. This is what "bounded-memory backpressure" means in practice.

Second, when the batch has room, `ReadGroup` pulls up to `maxCount-len(batch)`
new entries (blocking up to two seconds for them), and each is appended to the
batch, its byte size added, and its stream ID recorded for later ack.

> **Go concept — `proto.Size`.** `proto.Size(raw.Entry)` returns the number of
> bytes the protobuf message *would* occupy when serialized, without actually
> serializing it. We use it to track `batchBytes` cheaply so the "flush at N
> bytes" threshold reflects real wire/segment size, not Go's in-memory struct
> size (which is larger and irrelevant here). It's the size accounting you'd
> otherwise hand-roll from `ByteSizeLong()` in C++'s protobuf.

> **Go concept — nested `range` over a map of slices.** The outer `for device,
> raws := range msgs` iterates the returned `map[string][]Raw`; the inner loop
> walks each device's slice. Two levels of `range` flatten a map-of-slices into
> a stream of items — the read side of the same shape `acks` stores. (Recall map
> iteration order is randomized — fine, since we're just draining everything.)

The flush *decision* is two booleans: `full` (size threshold hit) or `due` (the
`interval` has elapsed *and* there's something to flush). `time.Since(a.lastFlush)`
is sugar for `time.Now().Sub(a.lastFlush)`. Batching by "size **or** time,
whichever trips first" bounds both latency (a trickle still flushes every
`interval`) and segment size (a flood flushes at `maxBytes`).

### 2.6 `flush` — write, *then* ack

```go
func (a *Archiver) flush(ctx context.Context) {
	if len(a.batch) == 0 {
		return
	}
	start := time.Now()
	m, err := a.writer.Write(ctx, a.batch)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Error("segment flush failed, batch retained", "entries", len(a.batch), "err", err)
		}
		return
	}
	for device, ids := range a.acks {
		if err := a.hot.Ack(ctx, device, ids...); err != nil && ctx.Err() == nil {
			// Segment is durable; a failed ack only risks re-archiving,
			// which query-side dedupe absorbs.
			a.log.Warn("ack failed after flush", "device", device, "err", err)
		}
	}
	a.metrics.SegmentFlushSeconds.Observe(time.Since(start).Seconds())
	// … more metrics, structured log …
	a.batch = nil
	a.batchBytes = 0
	a.acks = map[string][]string{}
	a.lastFlush = time.Now()
}
```

This is the safety-critical ordering, in code. `a.writer.Write` persists the
whole batch as one durable segment (doc 07). **Only if that succeeds** do we ack.
If the write fails, we `return` *without clearing the batch* — the entries stay
in `a.batch` and get retried next turn (that's what the §2.5 guard was catching),
and because they were never acked, a crash re-reads them too. The batch is
"retained" on failure; that single skipped reset is the entire backpressure and
at-least-once mechanism.

> **Go concept — variadic call with `ids...`.** `a.hot.Ack(ctx, device, ids...)`
> passes a whole `[]string` into a variadic parameter (`Ack(…, ids ...string)`).
> The `...` **spread** unpacks the slice into individual arguments — the mirror
> of declaring a variadic function. Without it you'd be passing one `[]string`
> where many `string`s are expected; the compiler would reject it.

Notice acks are **best-effort**: a failed ack only logs a warning and moves on,
because the segment is *already durable*. The worst case is that entries get
archived twice — and the query layer's dedup absorbs that. Losing data is
catastrophic; duplicating it is free. The code optimizes accordingly.

On success everything resets: `batch = nil` (a nil slice is a perfectly good
empty slice — `append` regrows it), `batchBytes = 0`, a **fresh** `acks` map, and
`lastFlush` bumped so the time-based trigger restarts.

### 2.7 `reclaim` — crash recovery on startup

```go
func (a *Archiver) reclaim(ctx context.Context) {
	devices, err := a.hot.Devices(ctx)
	if err != nil {
		return
	}
	for _, d := range devices {
		if err := a.hot.EnsureGroup(ctx, d); err != nil {
			continue
		}
		raws, err := a.hot.ClaimStale(ctx, d, a.consumer, time.Minute)
		if err != nil || len(raws) == 0 {
			continue
		}
		a.log.Info("reclaimed unarchived entries", "device", d, "entries", len(raws))
		for _, raw := range raws {
			a.batch = append(a.batch, raw.Entry)
			a.batchBytes += proto.Size(raw.Entry)
			a.acks[d] = append(a.acks[d], raw.ID)
		}
	}
}
```

`reclaim` runs *once*, at the very top of `Run`, before the loop. `ClaimStale`
wraps Redis's `XAUTOCLAIM`: it finds entries that some consumer read but never
acked and whose idle time exceeds one minute — i.e. entries orphaned by a
*previous crashed run* — and reassigns them to this consumer. They're loaded
straight into the batch, so the first normal flush archives them. Without this,
a crash mid-flush would leave those entries stuck "pending" in the group forever,
never re-delivered by a plain `ReadGroup` (which only returns *new* messages).
`reclaim` is what makes at-least-once actually deliver the "at least" after a
crash.

### 2.8 `finalFlush` — graceful drain with a fresh context

```go
// finalFlush runs during shutdown with a fresh context so draining is not
// aborted by the cancellation that triggered it.
func (a *Archiver) finalFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a.flush(ctx)
}
```

This is subtle and important. `finalFlush` is called from the `ctx.Err() != nil`
branch of the loop — i.e. *because* the incoming context was cancelled. If it
reused that dead context, `a.writer.Write(ctx, …)` would fail instantly
(the context is already done) and the in-flight batch would be lost. So it mints
a **brand-new** context.

> **Go concept — a fresh `context.WithTimeout` for cleanup.**
> `context.Background()` is the empty root context that is never cancelled.
> `context.WithTimeout(parent, 15*time.Second)` derives a child that *auto-cancels
> after 15 seconds*, returning the context and a `cancel` function. Deriving from
> `Background()` — not from the dying `ctx` — deliberately **detaches** the drain
> from the shutdown signal, giving it a fresh 15-second budget to finish. This is
> the standard "shutdown gets its own deadline" pattern: the cancellation that
> *triggers* cleanup must not *abort* the cleanup.

> **Go concept — `defer cancel()`.** `WithTimeout` returns a `cancel` you must
> call to release the context's resources (a timer goroutine), even if the
> timeout never fires. `defer cancel()` schedules that call to run when the
> function returns, no matter which path it takes — the same `defer` cleanup
> idiom you saw guarding file handles in doc 07. Forgetting it is a real leak;
> `go vet` will flag it.

### 2.9 `pause` and `sleep` — backoff and the cancel-or-sleep pattern

```go
func (a *Archiver) pause(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	a.log.Error("archiver error, backing off", "err", err)
	a.sleep(ctx, time.Second)
}

func (a *Archiver) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
```

`pause` is the **backoff on error** helper: log the failure and wait a second
before the loop retries — so a persistent problem (Redis down, bucket
unreachable) produces one error per second, not a hot spin hammering the
dependency. It first checks `ctx.Err()` so a *shutdown* doesn't get logged as an
error or waited on.

`sleep` is where two Go concurrency primitives meet.

> **Go concept — timers.** `time.NewTimer(d)` creates a timer that sends the
> current time on its channel `t.C` once, after duration `d`. Unlike
> `time.Sleep`, a timer is a *value with a channel* you can wait on **alongside**
> other events. `defer t.Stop()` releases it early if we leave via the other
> branch, so we don't leak a pending timer. (There's no `time.Ticker` in this
> file — the periodic flush is driven by the `time.Since(lastFlush)` comparison
> in the loop, not by a ticker channel.)

> **Go concept — `select` and the cancel-or-sleep pattern.** `select` blocks
> until *one* of its cases can proceed, then runs that one. Here the two cases
> are "the context was cancelled" (`<-ctx.Done()`) and "the timer fired"
> (`<-t.C`). Whichever happens first wins. This is the canonical **interruptible
> sleep**: a plain `time.Sleep(d)` would block the full duration even if the
> program is trying to shut down, delaying exit by up to a second on every
> pending sleep. `select { case <-ctx.Done(): case <-t.C: }` sleeps *up to* `d`
> but wakes instantly on cancellation. Every wait in a long-running worker
> should be written this way. In C++ you'd emulate it with a condition variable
> and `wait_for` on a stop flag; Go bakes it into the language.

> **Go concept — `<-ch` and the done channel.** `ctx.Done()` returns a channel
> that is *closed* when the context is cancelled. A receive `<-ch` on a closed
> channel returns immediately — so a closed done-channel makes its `select` case
> ready. Reading from a channel is how goroutines synchronize; you'll create and
> send on channels yourself in doc 09.

---

## 3. Deep dives

### 3.1 At-least-once + ack-after-durable, end to end

Walk the failure timeline. Entries E1–E5 sit in a device stream. The archiver
`ReadGroup`s them — Redis marks them *pending* for consumer `archiver-1` but does
not remove them. The batch fills; `flush` calls `writer.Write`, which puts the
segment in the bucket. **Crash right here, before `Ack`.** On restart, `reclaim`
runs `XAUTOCLAIM`, finds E1–E5 still pending (idle > 1 min), and re-loads them.
The next flush writes them to cold storage a *second* time. Cold now holds two
segments containing E1–E5. Doc 10's query layer reads both, sees the same entry
IDs twice, and returns each once. Net effect: **exactly-once observation from
at-least-once storage**, with no two-phase commit, no distributed lock — just
"ack last" plus "dedup on read". The cost is occasional duplicate bytes in the
bucket; the benefit is that no single crash can ever lose an entry.

The inverse ordering — ack *then* write — would be a data-loss bug: a crash
between them would drop E1–E5 forever, because they'd be acked out of Redis but
never made it to cold. The code's ordering is not an accident; it's the invariant.

### 3.2 Bounded memory during a bucket outage

Suppose the object store is down for an hour. `writer.Write` fails every time, so
`flush` retains the batch. The §2.5 guard sees `len(a.batch) >= a.maxCount`,
retries the flush, sleeps a second, and `continue`s — it **never reads more**.
Memory is pinned at one batch (`maxCount` entries / `maxBytes` bytes), no matter
how long the outage lasts, because the unread backlog accumulates in *Redis*,
which is already durable and already bounded by the hot tier's own retention.
When the bucket recovers, the retained batch flushes, the guard clears, and
normal reading resumes to burn down the backlog. This is backpressure done right:
the slow consumer (cold) throttles the fast reader (the loop) via a simple "don't
read while full" rule, and the buffer of last resort is the durable log, not RAM.

---

## 4. Idioms & gotchas

- **Context is the first argument, always.** `Run(ctx)`, `flush(ctx)`,
  `sleep(ctx, d)` — any function that blocks or does I/O takes `ctx` first. It's
  a convention the whole ecosystem relies on; follow it.
- **Never `time.Sleep` in a cancellable worker.** Use the `select { case
  <-ctx.Done(): case <-t.C: }` pattern so shutdown isn't delayed. A stray
  `time.Sleep` is the classic "why does my service take 30s to stop?" bug.
- **Cooperative cancellation only.** Go can't kill a goroutine. If the loop
  stops checking `ctx.Err()` / `ctx.Done()`, it runs forever. Every long wait and
  every loop turn must give cancellation a chance to win.
- **Reset state *after* success, retain on failure.** `flush` clears `batch`,
  `batchBytes`, and `acks` only on the success path. The skipped reset on failure
  *is* the retry mechanism — deleting that early-return would silently lose data.
- **Shutdown needs its own context.** Deriving cleanup from the cancelled
  context aborts the cleanup. `context.WithTimeout(context.Background(), …)` plus
  `defer cancel()` gives the drain a fresh, bounded budget.
- **`append` to a missing map key just works.** `a.acks[d] = append(a.acks[d],
  id)` needs no "does the key exist?" check, because `append(nil, …)` allocates.
  But you must initialize the *map itself* (in `New`) before writing any key.
- **Pointer receiver or your mutations vanish.** Every method mutates the
  worker, so every receiver is `*Archiver`. A value receiver here would be a
  quiet correctness bug, not just a style choice.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why is `writer.Write` called *before* `hot.Ack`, and what exactly
   breaks if you swap the order? Trace a crash between the two calls in each
   ordering.
2. **Recall.** Why does `finalFlush` build a new context from
   `context.Background()` instead of reusing the `ctx` passed to `Run`? What
   would `writer.Write` return if it used the cancelled one?
3. **Apply.** Rewrite `sleep` using `time.Sleep(d)` and describe, concretely,
   how process shutdown latency changes. Then explain why the `select` version is
   strictly better.
4. **Apply.** The backoff in `pause` is a flat one second. Change it to
   exponential backoff (1s, 2s, 4s, capped at 30s) that resets after a
   successful turn. Where does the backoff state have to live, and why the struct?
5. **Extend.** `acks` is a `map[string][]string`. Add a metric for the number of
   distinct devices in each flushed batch (hint: `len(a.acks)` just before the
   reset). Which existing line already computes something similar for the log?
6. **Hero.** Today one `consumer` ("archiver-1") does all the work. Sketch how
   you'd run *N* archiver goroutines sharing the group for throughput. What must
   `reclaim`'s idle threshold account for so two live workers don't steal each
   other's in-flight entries? (Think about what `XAUTOCLAIM`'s min-idle protects.)

---

## 6. Recap & next

You've now seen a Go program that *lives*: a `for {}` event loop driven by
`Run(ctx)`, cancelled cooperatively via `context`, waiting on interruptible
timers, and carrying mutable batch/ack state across turns in a pointer-receiver
struct. You've seen the durability contract that makes it safe — write the
segment, *then* ack; retain on failure for bounded-memory backpressure; reclaim
orphans on startup; and drain on shutdown under a fresh deadline — and how
at-least-once storage plus downstream dedup yields exactly-once effect.

**Next:** [09 — ingest](09-ingest.md), where you meet the other half of the
concurrency story: launching **goroutines**, guarding shared state with
`sync.Mutex`, and coordinating producers and consumers over **channels** and
non-blocking `select` sends — the write path that fills the very streams this
archiver drains.
