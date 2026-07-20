# Component 14 — `cmd/devlogd` (the composition root)

> **Role:** the capstone — load config, construct **every** component, inject
> dependencies, and run all long-lived goroutines under one supervisor that
> starts fail-fast and shuts down gracefully. | **Source:** `cmd/devlogd/main.go`

**Where this sits in the journey:** this is the finish line. Every earlier doc
studied one package in isolation and noticed it "takes its dependencies as
parameters." *This* file is where those parameters come from. Prerequisites:
**all of docs [01](01-proto-contract.md)–[13](13-cli-tools.md)** — we name each
of them here. New Go territory: **structured concurrency** with `errgroup`,
signal-driven context, and `sync/atomic`.

## What you'll master

- **Go:** the `main → run(...) error` pattern; `flag` for `-config`; the
  **composition root / dependency-injection** idea (the one place that
  constructs concretes → zero globals); **`signal.NotifyContext`** turning
  SIGINT/SIGTERM into cancellation; **`golang.org/x/sync/errgroup`** and
  **structured concurrency** (`WithContext`, `g.Go`, `g.Wait`); a shutdown
  goroutine on `<-ctx.Done()`; `net.Listen`; **`sync/atomic`** (`atomic.Bool`);
  `defer` ordering.
- **Domain:** startup order, fail-fast, graceful drain, readiness.

---

## 1. Orientation

A large program needs exactly one place where abstract pieces become concrete
and get plugged together — the **composition root**. In devlogd that is
`run(cfgPath)`. Read top to bottom it is the system's wiring diagram: crypto →
licensing → storage tiers → pipeline → planes → supervisor. Nothing here is
clever business logic; the cleverness is *assembly and lifecycle*. Because every
component took its dependencies as constructor parameters, this file can wire
them with **no global variables at all** — which is what made each of them
independently testable in [TESTING.md](../TESTING.md).

---

## 2. Guided code walkthrough

### 2.1 `main` and `run`

```go
func main() {
	cfgPath := flag.String("config", "config/devlogd.yaml", "path to YAML config")
	flag.Parse()
	if err := run(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "devlogd:", err)
		os.Exit(1)
	}
}
```

> **Go concept — the `main`/`run` split (recap, [13](13-cli-tools.md)).** `main`
> parses the one flag and delegates to `run(...) error`, so all real logic uses
> normal error flow and — crucially — the `defer`s inside `run` actually execute
> (they would not if `main` called `os.Exit` directly). `flag.String` returns a
> `*string` filled by `flag.Parse()`.

### 2.2 Config, logger, metrics, and the signal context

```go
func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := telemetry.NewLogger(cfg.Log.Level)
	metrics := telemetry.NewMetrics()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
```

> **Go concept — dependency injection begins.** `config.Load` ([02](02-config.md))
> is the first construction; its result feeds the logger ([03](03-telemetry.md))
> and everything downstream. `metrics` is the single injected `*Metrics` struct
> that every component will share (no package-level metric globals).

> **Go concept — `signal.NotifyContext`.** This derives a `context.Context` from
> the root `context.Background()` that is **cancelled when the process receives
> `os.Interrupt` (Ctrl-C) or `SIGTERM`** (what `systemctl stop` and the C++
> supervisor's `stop()` send). So an OS signal becomes a plain context
> cancellation that propagates everywhere — the same `ctx` the archiver
> ([08](08-archive.md)), broker ([11](11-broker.md)), and gRPC streams
> ([12](12-grpcapi.md)) already watch. `defer stop()` releases the signal handler
> on return.

### 2.3 Fail-fast construction of crypto, licensing, storage

```go
	signer, err := sign.LoadSigner(cfg.Signing.KeyFile, cfg.Signing.KeyID)
	if err != nil {
		return err
	}
	verifier := sign.NewVerifier()
	verifier.Add(cfg.Signing.KeyID, signer.Public())
	issuerPub, err := sign.LoadPublicPEM(cfg.License.IssuerPubFile)
	// … online validator if cfg.License.Mode == "online" …
	sessions := license.NewManager(license.NewVerifier(issuerPub), online, metrics, log)

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis unreachable at %s: %w", cfg.Redis.Addr, err)
	}
	hotStore := hot.New(rdb, cfg.Redis.HotRetention.Std())
	objectStore, err := cold.NewMinio(ctx, cfg.S3.Endpoint, cfg.S3.AccessKey, cfg.S3.SecretKey,
		cfg.S3.UseTLS, cfg.S3.Bucket)
	if err != nil {
		return fmt.Errorf("object store unreachable at %s: %w", cfg.S3.Endpoint, err)
	}
```

> **Go concept — fail-fast, expressed as early returns.** Each construction that
> can fail returns immediately with a wrapped error (`%w`, [02](02-config.md)):
> a missing signing key, an unreachable Redis (`rdb.Ping`), or an unreachable
> MinIO aborts startup *before any listener opens*. This is why a misconfigured
> devlogd dies in the first second with a precise message rather than at 3 a.m.
> mid-request. The C++ supervisor ([../deploy/embed](../deploy/embed/README.md))
> relies on exactly this: it detects the early child exit instead of hanging.

> **Go concept — `defer rdb.Close()` and defer ordering.** `defer` schedules
> cleanup to run when `run` returns, in **LIFO** order (last deferred, first
> run). `defer stop()` was registered first, `defer rdb.Close()` second, so on
> exit Redis closes, then the signal handler is released. `defer` is Go's RAII
> substitute (you've seen it since [07 — cold](07-cold.md)); here it guarantees
> the Redis connection is released on every return path.

This block is the composition root at work: `signer`/`verifier`
([04](04-sign.md)) and `sessions` ([05](05-license.md)) are built and will be
injected; `hotStore` ([06](06-hot.md)) and `objectStore` ([07](07-cold.md)) are
the storage tiers.

### 2.4 Wiring the pipeline and both planes

```go
	hub := ingest.NewHub()
	pipeline := ingest.NewPipeline(hotStore, signer, hub, metrics, log)
	archiver := archive.New(hotStore, cold.NewWriter(objectStore),
		cfg.Archive.FlushInterval.Std(), cfg.Archive.MaxBatchBytes, cfg.Archive.MaxBatchEntries, metrics, log)
	engine := query.New(hotStore, cold.NewReader(objectStore), signer, verifier,
		cfg.Query.MaxResults, cfg.Query.DefaultLookback.Std())

	// … build mqttTLS / grpcTLS via config.ServerTLS …
	brk, err := broker.New(cfg.MQTT.Listen, mqttTLS, pipeline, sessions, metrics, log)
	grpcServer := grpcapi.NewServer(grpcTLS, sessions,
		grpcapi.NewService(engine, hub, hotStore, log), metrics, log)

	var ready atomic.Bool
	httpSrv := telemetry.NewHTTP(cfg.HTTP.Listen, metrics.Registry, ready.Load, log)
```

> **Go concept — the wiring diagram, made literal.** Read the injections:
> `NewPipeline(hotStore, signer, hub, …)` ([09](09-ingest.md)) — the ingest core
> gets the hot store, the signer, and the live hub. `archive.New(hotStore,
> cold.NewWriter(objectStore), …)` ([08](08-archive.md)) — the archiver drains
> hot into cold. `query.New(hotStore, cold.NewReader(objectStore), signer,
> verifier, …)` ([10](10-query.md)) — the engine reads both tiers.
> `broker.New(..., pipeline, sessions, …)` ([11](11-broker.md)) feeds the
> pipeline; `grpcapi.NewServer(..., NewService(engine, hub, hotStore, …))`
> ([12](12-grpcapi.md)) exposes the engine and hub. **Every arrow in
> [COMPONENTS.md](../COMPONENTS.md)'s diagram is one argument here.**

> **Go concept — `sync/atomic` and passing a method value.** `var ready
> atomic.Bool` is a lock-free boolean. `ready.Load` (note: no `()`) is a **method
> value** — the method bound to `ready`, passed as the `func() bool` the health
> server ([03](03-telemetry.md)) calls to answer `/readyz`. It flips to `true`
> only after everything is constructed (below), and it's read from the HTTP
> handler's goroutine while `run`'s goroutine writes it — a data race for a plain
> `bool`, which `atomic.Bool` makes safe.

### 2.5 Structured concurrency with `errgroup`

```go
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return brk.Run(ctx) })
	g.Go(func() error { return archiver.Run(ctx) })
	g.Go(func() error {
		return hotStore.RunJanitor(ctx, 5*time.Minute, func(err error) {
			log.Error("hot retention trim failed", "err", err)
		})
	})
	g.Go(func() error { return httpSrv.Run(ctx) })

	lis, err := net.Listen("tcp", cfg.GRPC.Listen)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	g.Go(func() error { return grpcServer.Serve(lis) })
	g.Go(func() error {
		<-ctx.Done()
		grpcServer.GracefulStop()
		return nil
	})

	ready.Store(true)
	log.Info("devlogd up", "mqtt", cfg.MQTT.Listen, "grpc", cfg.GRPC.Listen, …)
	err = g.Wait()
	log.Info("devlogd stopped")
	return err
}
```

> **Go concept — goroutines, recapped and supervised.** `go f()` starts a
> concurrent function ([09 — ingest](09-ingest.md)). Raw goroutines are hard to
> supervise: if one fails, who tells the others? **`errgroup`** solves this.
> `errgroup.WithContext(ctx)` returns a group `g` and a **new derived `ctx`**.
> Each `g.Go(func() error { … })` runs a component's `Run(ctx)` on its own
> goroutine.

> **Go concept — structured concurrency.** `g.Wait()` blocks until **all**
> goroutines return, and yields the **first non-nil error**. The magic: when any
> goroutine returns an error (or the signal cancels the parent), `errgroup`
> **cancels the group's `ctx`**, which every `Run(ctx)` is watching — so one
> failure (or one Ctrl-C) unwinds the *entire* process cleanly. There are no
> orphaned goroutines and no manual coordination. This is the antidote to the
> "fire-and-forget goroutine that outlives its owner" bug; lifetimes are nested,
> hence *structured* concurrency. Contrast a C++ program juggling threads and
> condition variables by hand.

> **Go concept — a goroutine as a shutdown coordinator.** gRPC's `Serve(lis)`
> blocks until the server stops, and `GracefulStop()` must be called from
> *another* goroutine. So one `g.Go` runs `Serve`, and a second `g.Go` waits on
> `<-ctx.Done()` and then calls `GracefulStop()` (drain in-flight RPCs). When the
> signal fires, `ctx` cancels, this goroutine stops the gRPC server, `Serve`
> returns, and `g.Wait()` unblocks. `net.Listen("tcp", addr)` opens the socket;
> a failure here fails fast before the group starts.

> **Go concept — the readiness flip.** `ready.Store(true)` happens **after** all
> construction and just before `g.Wait()`, so `/readyz` reports 200 only when the
> service is genuinely able to serve — the signal Kubernetes-style probes and the
> C++ supervisor gate on.

---

## 3. Deep dives

### 3.1 Structured concurrency = signals + errgroup + the `Run(ctx)` convention

Three ideas compose into devlogd's whole lifecycle model:

1. **Every long-lived component exposes `Run(ctx) error`** and returns promptly
   when `ctx` is cancelled (you saw this shape in the broker, archiver, HTTP
   server, and janitor).
2. **`signal.NotifyContext`** makes an OS signal cancel the root context.
3. **`errgroup.WithContext`** ties N components to one context and one error
   channel: *first* error or signal → cancel → everyone drains → `g.Wait()`
   returns.

The payoff is a single, correct answer to "how does this process stop?": one
Ctrl-C, or one fatal component failure, cancels `ctx`; the archiver's
`finalFlush` drains the last batch ([08](08-archive.md)), the gRPC server
gracefully stops, and `run` returns the originating error (or `nil`). No
component needs to know about any other's shutdown — they all just watch `ctx`.

### 3.2 The composition root as the map of everything you learned

`run` is the table of contents of this whole tutorial. Walk it and you can point
to every doc:

| Line in `run` | Component | Doc |
| --- | --- | --- |
| `config.Load` | configuration | [02](02-config.md) |
| `telemetry.NewLogger/NewMetrics/NewHTTP` | observability | [03](03-telemetry.md) |
| `sign.LoadSigner/NewVerifier` | crypto core | [04](04-sign.md) |
| `license.NewManager` | authentication | [05](05-license.md) |
| `hot.New` | hot tier | [06](06-hot.md) |
| `cold.NewMinio/NewWriter/NewReader` | cold tier | [07](07-cold.md) |
| `archive.New` | archiver | [08](08-archive.md) |
| `ingest.NewHub/NewPipeline` | ingest | [09](09-ingest.md) |
| `query.New` | query engine | [10](10-query.md) |
| `broker.New` | MQTT plane | [11](11-broker.md) |
| `grpcapi.NewServer/NewService` | gRPC plane | [12](12-grpcapi.md) |

Because construction is explicit and localized, swapping an implementation
(a different object store, a fake in a test) is a one-line change *here* and
nowhere else — the definition of a healthy composition root.

---

## 4. Idioms & gotchas

- **One composition root, zero globals.** Construct concretes in `main`/`run`,
  inject everywhere else. It is what makes the rest of the codebase testable.
- **`Run(ctx) error` for every long-lived component** so `errgroup` can supervise
  them uniformly.
- **`errgroup.WithContext` returns a *new* ctx** — use that one in `g.Go`
  closures; the first error cancels it and unwinds the rest.
- **Blocking `Serve` + a `<-ctx.Done()` goroutine calling `GracefulStop`** is the
  standard way to shut a blocking server down.
- **Flip readiness last** (`atomic.Bool`) so `/readyz` never lies.
- **`defer` is LIFO**; order your `defer stop()` / `defer rdb.Close()`
  accordingly.
- **Fail fast at construction** — ping/reach dependencies before opening
  listeners, and wrap errors with context.

---

## 5. Exercises (zero → hero)

1. **Recall.** What does `errgroup.WithContext` return, and what happens to the
   other goroutines when one `g.Go` function returns an error?
2. **Recall.** Why is `ready.Store(true)` placed just before `g.Wait()` and not
   earlier?
3. **Apply.** Add a new background component `foo` with a `Run(ctx) error`
   method and supervise it. What single line makes it participate in graceful
   shutdown?
4. **Apply.** The gRPC server needs a separate goroutine to stop it. Explain why
   you can't just call `GracefulStop()` right after `Serve(lis)` in the same
   `g.Go`.
5. **Hero.** Trace a `SIGTERM` from the kernel to the final `log.Info("devlogd
   stopped")`: name every hop (signal → context → errgroup → each `Run` → final
   archiver flush → `g.Wait`). Then explain how the C++ supervisor's `stop()`
   ([../deploy/embed](../deploy/embed/README.md)) drives the same path.

---

## 6. Recap & next — you made it

You can now read `cmd/devlogd/main.go` as a wiring diagram and a lifecycle
contract: config and dependencies constructed fail-fast, injected without a
single global, run under **structured concurrency** where one signal or one
error unwinds the whole process gracefully, with readiness surfaced only when
the service is truly up.

That completes the path. From proto schemas ([01](01-proto-contract.md)) through
types, interfaces, errors, concurrency, streaming, and whole-system assembly,
you have met — in real, shipping code — the Go that takes you from zero to hero.
Revisit any component from [the index](README.md), and pair this with
[../COMPONENTS.md](../COMPONENTS.md) (architecture) and
[../TESTING.md](../TESTING.md) (how it's all verified).
