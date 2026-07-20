# Component 03 — `internal/telemetry`

> **Role:** own devlogd's *self*-observability — structured JSON logs,
> Prometheus metrics, and the HTTP endpoint that serves `/metrics`, `/healthz`,
> and `/readyz` with graceful shutdown. | **Source:**
> `internal/telemetry/logging.go`, `internal/telemetry/metrics.go`,
> `internal/telemetry/http.go`

**Where this sits in the journey:** doc 02 taught you the *shape* of a Go
package — types, methods, structs, errors. This component reuses all of that
(you'll see `*Metrics` structs, methods, and `error` returns without re-teaching
them) and spends its depth on three genuinely new ideas: **functions as
values** (closures passed as arguments), the **`net/http` server**, and
**concurrency for the first time** — a goroutine, a channel, and `select`
cooperating to shut a server down cleanly. Prerequisite: [02 — config](02-config.md)
(the `Log.Level` string that lands here comes from there).

## What you'll master

- **Go:** `log/slog` structured logging (JSON handler, levels, `UnmarshalText`);
  **function values, closures, and first-class functions** (the `ready func() bool`
  parameter and the inline `func(w, r){…}` handlers); the **`net/http` server**
  — `ServeMux`, `Handle`/`HandleFunc`, `http.HandlerFunc`, `ResponseWriter`,
  `WriteHeader`; **your first goroutine, channel, and `select`**; **graceful
  shutdown** with `context.Context` + a buffered channel + `srv.Shutdown`;
  `errors.Is(err, http.ErrServerClosed)`; the Prometheus client
  (`NewRegistry`, `promauto.With`, `CounterVec`/`Histogram`/`Gauge`).
- **Domain:** liveness vs readiness (`/healthz` vs `/readyz`), RED-style
  metrics, machine-parseable JSON logs, and **dependency injection over
  package-level globals** for test isolation.

---

## 1. Orientation

A service you cannot observe is a service you cannot operate. `internal/telemetry`
is devlogd watching itself, and it answers three operational questions:

- **"What is happening?"** — `NewLogger` builds a `slog.Logger` that emits one
  JSON object per line, so a log shipper (Loki/ELK) can parse fields without
  regexes.
- **"How is it performing?"** — `NewMetrics` builds a `*Metrics` struct holding
  every Prometheus counter, gauge, and histogram devlogd exports, backed by a
  *fresh registry* it owns.
- **"Is it alive, and is it ready?"** — `NewHTTP` wires a tiny HTTP server that
  serves `/metrics` (the scrape endpoint), `/healthz` (liveness), and `/readyz`
  (readiness), and `Run` keeps it alive until its context is cancelled, then
  drains in-flight requests before returning.

The package deliberately owns *no* business logic. It is pure plumbing that
every other component reaches for — which is exactly why the way it is *wired*
(injected structs, not globals) matters so much.

---

## 2. Guided code walkthrough

### 2.1 `logging.go` — structured logs from a string

```go
// Package telemetry owns the service's own observability: structured logs,
// Prometheus metrics, and the health/metrics HTTP endpoint.
package telemetry

import (
	"log/slog"
	"os"
)

// NewLogger returns a JSON slog logger; JSON keeps the service's own logs
// machine-parseable for Loki/ELK pipelines.
func NewLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
```

> **Go concept — `log/slog`, structured logging.** `slog` is Go's standard
> **structured** logger (Go 1.21+). Instead of formatting a human sentence, you
> log a *message plus key/value pairs* — `log.Info("ingested", "device", id,
> "bytes", n)` — and a **handler** decides the wire format. Here
> `slog.NewJSONHandler` renders each record as a JSON object
> (`{"time":…,"level":"INFO","msg":"ingested","device":…}`). A machine can index
> those fields directly; contrast the printf-style logging you might reach for in
> C++, where downstream you'd write brittle regexes to pull values back out.

> **Go concept — handlers are an interface.** `slog.New` takes a `slog.Handler`
> (an interface, per doc 02's implicit-satisfaction rule). The JSON handler is
> one implementation; `slog.NewTextHandler` is another; you can write your own.
> The `*slog.Logger` returned is the *front end* every caller uses — swapping the
> handler changes the output format without touching a single call site. That
> separation of "what to log" from "how to render it" is the whole point of the
> package.

> **Go concept — `UnmarshalText` and the zero-value fallback.** `slog.Level` is
> an integer type that satisfies `encoding.TextUnmarshaler` — the same
> "type teaches itself to parse a string" pattern you saw with `Duration` in doc
> 02, but from the standard library. `l.UnmarshalText([]byte("debug"))` parses
> `"debug"`/`"info"`/`"warn"`/`"error"` into the right level. Note the receiver
> `l` starts at its **zero value**, which happens to be `LevelInfo` (0) — so if
> parsing fails we simply *keep* that safe default and swallow the error rather
> than crash. A bad log-level string should never take the process down.

> **Go concept — `[]byte(level)` conversion.** `UnmarshalText` wants a
> `[]byte`, not a `string`; `[]byte(level)` is an explicit **conversion**
> (strings and byte slices are distinct types). In Go a `string` is immutable
> UTF-8 bytes, so this conversion copies. You'll do the reverse (`string(b)`)
> constantly in doc 04.

### 2.2 `metrics.go` — a struct of metrics, not a pile of globals

```go
import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics is the single registry of everything devlogd exports to Prometheus.
// Passing this struct around (instead of package-level globals) keeps metric
// ownership explicit and tests isolated.
type Metrics struct {
	Registry *prometheus.Registry

	EntriesIngested     *prometheus.CounterVec // device, severity
	IngestBytes         prometheus.Counter
	IngestErrors        *prometheus.CounterVec // reason
	// … SanitizationPhase, Verification, HotAppendSeconds (Histogram) …
	MQTTConnections     prometheus.Gauge
	GRPCRequests        *prometheus.CounterVec // method, code
}
```

> **Go concept — the three metric shapes.** Prometheus has three primitives, and
> the field types name them: a **`Counter`** only ever goes up (bytes ingested,
> segments flushed); a **`Gauge`** goes up *and* down (currently-connected MQTT
> clients); a **`Histogram`** samples a distribution into buckets (append
> latency in seconds). A **`CounterVec`** is a *family* of counters keyed by
> **labels** — `EntriesIngested` is really "one counter per (device, severity)
> pair". The trailing comment `// device, severity` documents that label order,
> which you must match at the call site.

Now the constructor:

```go
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	f := promauto.With(reg)
	return &Metrics{
		Registry: reg,
		EntriesIngested: f.NewCounterVec(prometheus.CounterOpts{
			Name: "devlog_entries_ingested_total",
			Help: "Log entries accepted, by device and severity.",
		}, []string{"device", "severity"}),
		IngestBytes: f.NewCounter(prometheus.CounterOpts{
			Name: "devlog_ingest_bytes_total", Help: "Raw payload bytes accepted over MQTT.",
		}),
		HotAppendSeconds: f.NewHistogram(prometheus.HistogramOpts{
			Name: "devlog_hot_append_seconds", Help: "Latency of Redis appends.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 12),
		}),
		// … one field per metric, all registered against the same reg …
	}
}
```

> **Go concept — `NewRegistry` and an owned collection.** `prometheus.NewRegistry()`
> creates a *private* registry — a collection that knows how to gather every
> metric registered into it. The Prometheus client library ships a global
> default registry, but devlogd deliberately makes its own so nothing leaks in
> or out. `reg.MustRegister(collectors.NewGoCollector())` adds Go-runtime metrics
> (goroutines, GC, heap) to it. The `Must` prefix is a Go naming idiom: the
> function **panics** instead of returning an error, used only at startup where a
> failure is a programmer bug, not a runtime condition.

> **Go concept — `promauto.With(reg)` as a factory.** `promauto.With(reg)`
> returns a small factory value `f` whose `New…` methods both *create* a metric
> and *register it into `reg`* in one call. Without it you'd construct each
> metric and then call `reg.MustRegister` on it separately — easy to forget.
> `f` here is a plain value we hold in a local and call repeatedly; think of it
> as a pre-configured object, no globals involved.

> **Go concept — options-struct arguments.** Each field's value is a call taking
> a `prometheus.CounterOpts{…}` struct literal. This "options struct as the last
> argument" style replaces the long positional parameter lists or builder objects
> you'd see in C++ — you name only the options you care about; the rest take their
> zero values.

### 2.3 `http.go` — the server struct and its dependencies

```go
// HTTPServer serves /metrics, /healthz (liveness) and /readyz (readiness).
type HTTPServer struct {
	srv *http.Server
	log *slog.Logger
}

func NewHTTP(listen string, reg *prometheus.Registry, ready func() bool, log *slog.Logger) *HTTPServer {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	return &HTTPServer{
		srv: &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second},
		log: log,
	}
}
```

> **Go concept — functions are first-class values.** Look at the parameter
> `ready func() bool`. Its *type* is "function taking nothing and returning a
> bool" — a function is an ordinary value you can pass, store in a struct, and
> call. `NewHTTP` doesn't know or care *what* `ready` checks (in devlogd it reads
> an `atomic.Bool` set once startup finishes, see doc 14); it just calls
> `ready()` and trusts the answer. This is dependency injection in its lightest
> form: pass *behaviour*, not a concrete object. In C++ you'd reach for a
> `std::function`, a functor, or a virtual interface; in Go the bare function
> type does it, no wrapper needed.

> **Go concept — closures (function literals that capture).** The two
> `func(w http.ResponseWriter, _ *http.Request){ … }` values are **function
> literals** written inline. The `/readyz` one *captures* the `ready` variable
> from the enclosing `NewHTTP` scope — that captured binding survives after
> `NewHTTP` returns, because Go closures keep their captured variables alive
> (escape analysis moves them to the heap, doc 02). So the handler carries its
> dependency with it, no field or global required. The `_` for the `*http.Request`
> parameter is the **blank identifier**: "I must accept this argument but I don't
> use it."

> **Go concept — `ServeMux`, `Handle` vs `HandleFunc`.** `http.NewServeMux()` is
> a **request router**: it maps URL path patterns to handlers. A *handler* is
> anything satisfying `http.Handler` (an interface with one method,
> `ServeHTTP(w, r)`). `mux.Handle(pattern, h)` registers a value that already
> *is* an `http.Handler` — `promhttp.HandlerFor(reg, …)` returns one that
> serves `reg`'s metrics in the Prometheus text format. `mux.HandleFunc(pattern,
> fn)` is the convenience form: give it a plain `func(w, r)` and it wraps it for
> you.

> **Go concept — `http.HandlerFunc`, the adapter.** How does a bare function
> become an `http.Handler`? The standard library defines
> `type HandlerFunc func(ResponseWriter, *Request)` with a method
> `func (f HandlerFunc) ServeHTTP(w, r) { f(w, r) }`. So a *function type* has a
> method that just calls the function — turning any matching function into an
> interface value. `HandleFunc` does that conversion under the hood. This
> "method on a function type" trick is a peculiarly Go pattern worth internalising:
> it lets a function satisfy an interface.

> **Go concept — `ResponseWriter` and `WriteHeader`.** `w http.ResponseWriter`
> is the *interface* you write the reply through. `w.WriteHeader(http.StatusOK)`
> sends the HTTP status line (200); `http.StatusServiceUnavailable` is 503.
> `WriteHeader` may be called **at most once**, and once you call `w.Write(body)`
> the header is sent automatically as 200 if you haven't. The health handlers
> here send *only* a status code — an empty body is all a probe needs. Note the
> early `return` after the 200 in `/readyz`: without it, control would fall
> through and try to write a second header.

> **Go concept — configuring the server via a struct literal.** `&http.Server{
> Addr: …, Handler: mux, ReadHeaderTimeout: 5*time.Second}` builds the server by
> naming fields. `ReadHeaderTimeout` caps how long a slow client may dribble in
> its request headers — a cheap defence against a class of slow-loris DoS. Every
> field left unset takes its zero value; you opt in to the knobs you need.

### 2.4 `Run` — serve, then shut down gracefully

```go
// Run serves until ctx is cancelled, then shuts down gracefully.
func (h *HTTPServer) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- h.srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return h.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
```

This tiny method is the most concept-dense code in the whole doc — it is your
first goroutine, first channel, and first `select`, all in service of a real
problem: *how do you stop a blocking server on command without dropping requests?*

> **Go concept — goroutines.** `go func(){ … }()` launches the function as a
> **goroutine**: a concurrently-scheduled, extremely cheap thread of execution
> (a few KB of stack, multiplexed onto OS threads by the Go runtime). We need one
> because `h.srv.ListenAndServe()` **blocks forever** — it only returns when the
> server stops. Running it in a goroutine lets `Run` continue past that line and
> wait for *either* the server to fail *or* the context to be cancelled.

> **Go concept — channels, and why this one is buffered.** `make(chan error, 1)`
> creates a **channel** that carries `error` values between goroutines — Go's
> typed, synchronised pipe ("Do not communicate by sharing memory; share memory
> by communicating"). The `1` makes it **buffered**: the goroutine can send one
> value and move on *even if no one is receiving yet*. That buffer is essential
> here. If `ctx` is cancelled we take the shutdown branch and *never* read
> `errCh`; with an unbuffered channel the background goroutine would block
> forever on its send when `ListenAndServe` finally returns — a **goroutine
> leak**. The size-1 buffer lets it deliver its result and exit regardless.

> **Go concept — `select`.** `select` waits on multiple channel operations and
> proceeds with whichever is **ready first** (if several are ready it picks one
> at random). It is the concurrency sibling of `switch`. Here the two cases are
> the two ways this server can end:
> - `<-ctx.Done()` — the context was cancelled (someone asked us to stop). We
>   begin a graceful shutdown.
> - `err := <-errCh` — `ListenAndServe` returned on its own (a bind failure, or
>   a shutdown we triggered). We inspect the error.

> **Go concept — `context.Context` for cancellation.** `ctx context.Context` is
> Go's standard carrier for **cancellation and deadlines** across API
> boundaries. `ctx.Done()` returns a channel that is *closed* when the context is
> cancelled; a receive on a closed channel returns immediately, which is why
> `<-ctx.Done()` is the "please stop" signal. The parent (doc 14) cancels this
> context on SIGTERM, and every long-running component watching the same context
> winds down together. Contrast a C++ world of shutdown flags and condition
> variables — `context` standardises the whole pattern.

> **Go concept — graceful shutdown with a fresh timeout context.** On cancel we
> build `context.WithTimeout(context.Background(), 5*time.Second)`: a *new*
> context that expires in five seconds. We must use `context.Background()` (a
> fresh root), **not** the already-cancelled `ctx`, or shutdown would abort
> instantly. `h.srv.Shutdown(shutdownCtx)` then stops accepting new connections
> and **waits for in-flight requests to finish** — up to five seconds, after
> which it forces the remaining ones closed. `defer cancel()` releases the
> timeout's resources whichever way the function exits (the `defer` pattern gets
> a full treatment in doc 07).

> **Go concept — `errors.Is(err, http.ErrServerClosed)`.** When `Shutdown`
> stops the server, the blocked `ListenAndServe` returns the **sentinel error**
> `http.ErrServerClosed`. That is the *expected, normal* outcome of a clean stop,
> not a failure — so we test for it with `errors.Is` and translate it to `nil`
> ("no error"). `errors.Is` unwraps the chain (`%w` from doc 02) to compare
> against a known sentinel value, the correct way to ask "is this *that specific*
> error?" rather than `==` on the string. Any *other* error (e.g. "address
> already in use") is returned to the caller as a real failure.

---

## 3. Deep dives

### 3.1 Dependency injection vs package-level globals

The single most important design decision in this component is invisible at
first glance: `Metrics` is a **struct you construct and pass around**, and the
Prometheus registry inside it is **created fresh** by `NewMetrics`. The lazy
alternative — the one a C++ instinct (or a Go beginner) reaches for — is
package-level globals:

```go
// The tempting anti-pattern (devlogd does NOT do this):
var EntriesIngested = promauto.NewCounterVec( … ) // registers into the GLOBAL registry
```

Globals feel convenient — any file can bump `EntriesIngested` without threading a
parameter through. But they carry two costs that matter in a real service:

1. **They register into one shared, process-wide registry.** Prometheus panics
   if you register two metrics with the same name. Fine in production (one
   process, one registration) but poisonous in tests: the *second* test that
   imports the package trips a "duplicate metrics collector registration" panic,
   or two tests silently share the same counter, so test A's increments corrupt
   test B's assertions.
2. **They hide dependencies.** A function that reads a global has an *invisible*
   input — its signature doesn't reveal it touches metrics, and you can't
   substitute a test double.

devlogd's approach fixes both. Each call to `NewMetrics()` builds its **own**
`prometheus.NewRegistry()`, so a test writes `m := NewMetrics()`, exercises the
code with `m`, and reads `m.EntriesIngested` back — a clean, isolated fixture,
and a *second* `NewMetrics()` in the next test is completely independent. No
global state, no registration collisions, no ordering dependencies between tests.
The dependency is now **explicit** in every signature that needs it
(`func ingest(…, m *telemetry.Metrics)`) and trivially mockable. This is the Go
answer to a question C++ often solves with singletons: prefer *passing* the
collaborator to *reaching for* it.

The same principle drives `ready func() bool` and `log *slog.Logger` being
parameters to `NewHTTP` rather than globals — the server is handed everything it
depends on, so it is self-contained and testable in isolation.

### 3.2 The goroutine + buffered-channel + `select` shutdown idiom

`Run` is a template you will meet again in nearly every long-lived component
(archive in doc 08, ingest in doc 09, the whole composition root in doc 14).
Read it as a four-part idiom:

1. **Offload the blocking call.** `ListenAndServe` never returns on its own, so
   it goes in a goroutine and reports its eventual result down a channel.
2. **Make the result channel buffered (size 1).** The goroutine must be able to
   deliver its one result and exit *even if the main path has stopped
   listening*. This is the difference between a clean exit and a leaked
   goroutine — the classic bug the beginner writes with an unbuffered channel.
3. **`select` on cancellation vs. self-termination.** Exactly two things can end
   the server: an external "stop" (`ctx.Done()`) or the server dying on its own
   (`errCh`). `select` waits for whichever comes first.
4. **Translate the expected stop into success.** A cancel triggers `Shutdown`
   with its own bounded timeout; a self-return that is `http.ErrServerClosed` is
   normal and becomes `nil`. Everything else is a genuine error.

Internalise this shape — "blocking worker in a goroutine, result on a buffered
channel, `select` between work and cancellation, graceful drain on cancel" is
*the* Go concurrency pattern, and this method is its smallest honest example in
the codebase.

### 3.3 Liveness, readiness, and RED metrics (the domain)

The two health endpoints answer *different* questions and an orchestrator
(Kubernetes) treats them differently:

- **`/healthz` = liveness.** "Is the process alive at all?" It always returns
  200 as long as the goroutine can serve. If a liveness probe fails, the
  orchestrator **restarts** the pod — a restart is assumed to fix it.
- **`/readyz` = readiness.** "Is the process ready to take traffic *right now*?"
  It calls the injected `ready()` and returns 200 or 503. If a readiness probe
  fails, the orchestrator **stops routing traffic** to the pod but leaves it
  running — during startup (still connecting to Redis, loading a license) or a
  transient dependency outage you want to be *live but not ready*. Conflating
  the two would cause needless restart loops.

The metrics follow the **RED method** — for each stream of work track its
**R**ate, **E**rrors, and **D**uration. `EntriesIngested` (rate) and
`IngestErrors` (errors) are counters; `HotAppendSeconds` (duration) is a
histogram so you can compute p99 latency, not just an average. Labels
(`device`, `severity`, `reason`) let you slice each series without exploding the
metric count. And because the logs are JSON, an incident responder can pivot
from a metric spike straight to the matching structured log lines by field —
that is the machine-parseable payoff of §2.1.

---

## 4. Idioms & gotchas

- **Buffer the result channel of a fire-and-forget goroutine.** Size 1 here is
  not arbitrary — it is what prevents the goroutine leaking when the `select`
  takes the *other* branch. Unbuffered would deadlock the goroutine's send.
- **Never pass the cancelled `ctx` to `Shutdown`.** Use a fresh
  `context.WithTimeout(context.Background(), …)`; reusing the dead context makes
  the graceful drain finish in zero seconds — i.e. not gracefully at all.
- **`http.ErrServerClosed` is success, not failure.** Always special-case it
  with `errors.Is` when your server can be shut down deliberately.
- **`WriteHeader` once, then `return`.** A second `WriteHeader` logs
  "superfluous response.WriteHeader call" and is ignored; forgetting the early
  `return` in a branch is the usual cause.
- **Fresh registry per `NewMetrics`, no globals.** It is what makes the metrics
  testable and dodges duplicate-registration panics. Resist `promauto.NewCounter`
  (global) in favour of `promauto.With(reg).NewCounter`.
- **Swallow the log-level parse error, keep the default.** Observability
  plumbing should degrade, never crash — a typo'd `LOG_LEVEL` yields `info`, not
  a dead process.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why is `errCh` created with `make(chan error, 1)` instead of
   `make(chan error)`? Describe precisely what leaks if you drop the `1`.
2. **Recall.** What is the difference between `/healthz` and `/readyz`, and what
   does an orchestrator do differently when each one fails?
3. **Apply.** Add a `/livez` alias for `/healthz` using `mux.HandleFunc`. Then
   rewrite the `/healthz` handler as a *named* function value assigned to a
   variable and register that same value under both paths — proving handlers are
   ordinary values.
4. **Apply.** `NewLogger` throws away the `UnmarshalText` error. Change it so an
   invalid level still defaults to info but *also* logs one warning line through
   the new logger saying which bad value it saw. Why must you build the logger
   before you can log the warning, and what does that imply about ordering?
5. **Extend.** Give `NewMetrics` a way to be tested: write a table-driven test
   that constructs two independent `*Metrics`, increments a counter on one, and
   asserts the other is untouched. Which property of the fresh-registry design
   makes this test possible without a cleanup step?
6. **Hero.** Wrap every handler in a middleware that records RED metrics per
   route: a `CounterVec{route, code}` and a `Histogram` of request duration.
   You'll need to capture the status code — the standard `ResponseWriter` won't
   tell you what was written, so implement a thin wrapper type that embeds
   `http.ResponseWriter` and overrides `WriteHeader` to remember the code.
   (Embedding is previewed here and taught fully in doc 05.)

---

## 6. Recap & next

You met Go's concurrency toolkit for the first time — goroutines, channels, and
`select` — and used it for a concrete, production-grade job: shutting a server
down without dropping requests. You saw that **functions are values** you can
pass (`ready func() bool`) and write inline as **closures** (the handlers), how
the `net/http` server is assembled from a `ServeMux` and handlers, and how
`slog` turns logging into structured data. Above all you saw *why* devlogd
injects a `*Metrics` struct with its own registry instead of reaching for
globals — explicit dependencies, isolated tests. Every later component leans on
this trio: a logger, a metrics struct, and a `Run(ctx)` that stops cleanly.

**Next:** [04 — sign](04-sign.md), where you drop into the crypto core — byte
slices, `crypto/*`, deterministic marshaling, and value-vs-pointer semantics
seen through Ed25519 signing.
