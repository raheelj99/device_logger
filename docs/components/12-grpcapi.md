# Component 12 — `internal/grpcapi`

> **Role:** the observation plane — serve `LogService` over TLS gRPC behind a
> uniform interceptor chain (panic-recovery → license auth → metrics).
> | **Source:** `internal/grpcapi/server.go`, `internal/grpcapi/service.go`

**Where this sits in the journey:** you have built the read/write internals;
this exposes them to the network. It brings together **middleware** (now as
explicit interceptor chains), **server streaming**, **context metadata**, and
**typed error codes**. Prerequisites: [02 — config](02-config.md) (TLS),
[05 — license](05-license.md) (the `Manager`), [09 — ingest](09-ingest.md) (the
`Hub`), [10 — query](10-query.md) (the `Engine`).

## What you'll master

- **Go:** the gRPC server; generated service interfaces and the
  `Unimplemented…Server` **embedding** for forward compatibility; **interceptors
  as middleware**, built by functions that *return* interceptors (closures over
  dependencies); reading request **metadata from `context.Context`**; the
  `status`/`codes` typed-error model; **server streaming** (`stream.Send`,
  `stream.Context()`); `defer` + `recover()` with **named return values** to
  neutralize panics; `strings.TrimPrefix`.
- **Domain:** license-gated (query feature) observation, five RPCs, live tail.

---

## 1. Orientation

gRPC generates a Go **interface** from the `.proto` service ([01](01-proto-contract.md)).
Our job is two-fold:

1. **`service.go`** — implement that interface: `Query`, `Tail`, `VerifyRange`,
   `ExportAuditReport`, `GetStats`. Each method translates a protobuf request
   into a call on the query engine or the hub, and translates results/errors
   back.
2. **`server.go`** — construct the gRPC server with **TLS credentials** and an
   **interceptor chain** that wraps *every* RPC with panic-recovery, license
   authorization, and metrics — so no handler can forget them.

---

## 2. Guided code walkthrough

### 2.1 Building the server with an interceptor chain (`server.go`)

```go
func NewServer(tlsCfg *tls.Config, sessions *license.Manager, svc *Service,
	metrics *telemetry.Metrics, log *slog.Logger) *grpc.Server {
	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(unaryRecover(log), unaryAuth(sessions), unaryMetrics(metrics)),
		grpc.ChainStreamInterceptor(streamRecover(log), streamAuth(sessions), streamMetrics(metrics)),
	)
	devicelogv1.RegisterLogServiceServer(gs, svc)
	return gs
}
```

> **Go concept — functional options.** `grpc.NewServer(...)` takes a variadic
> list of `grpc.ServerOption` values. `grpc.Creds(...)`,
> `grpc.ChainUnaryInterceptor(...)` etc. are *functions that return* options.
> This is the **functional-options pattern** — a Go idiom for configurable
> constructors: instead of a giant options struct or many constructors, you pass
> only the options you want, in any order. `credentials.NewTLS(tlsCfg)` wraps our
> `*tls.Config` ([02](02-config.md)) so the whole listener is TLS-only.

> **Go concept — interceptors = middleware.** gRPC has two RPC shapes (unary and
> streaming), so there are two interceptor chains. `ChainUnaryInterceptor(a, b,
> c)` runs `a` outermost, then `b`, then `c`, then the handler — like nested
> decorators. Applying the chain **once, at the server**, guarantees every
> current and future RPC is wrapped; there is no per-method opt-in to forget.
> (Contrast the MQTT hooks in [11 — broker](11-broker.md): same idea, expressed
> as a function chain rather than a callback interface.)

> **Go concept — registering the implementation against a generated interface.**
> `RegisterLogServiceServer(gs, svc)` accepts anything satisfying the generated
> `LogServiceServer` interface. The compiler checks `*Service` satisfies it —
> which is why the next section's embedding matters.

### 2.2 The service type and forward-compat embedding (`service.go`)

```go
type Service struct {
	devicelogv1.UnimplementedLogServiceServer
	engine *query.Engine
	hub    *ingest.Hub
	hot    *hot.Store
	log    *slog.Logger
}
```

> **Go concept — embedding for forward compatibility.** `Service` embeds the
> generated `UnimplementedLogServiceServer`, which provides a default
> "unimplemented" method for every RPC. If a *new* RPC is later added to the
> `.proto`, code still compiles: `Service` inherits the default (returns
> `codes.Unimplemented`) until you write the real method. Without this embed,
> adding an RPC would break the build of every server. Same mechanism as
> `HookBase` in [11 — broker](11-broker.md) — embed a base, override what you
> implement — here enforced as a gRPC convention.

> **Go concept — dependencies as fields.** The engine, hub, and hot store are
> injected via `NewService(...)` and stored as unexported fields — the same DI
> discipline as everywhere else.

### 2.3 A unary handler — `VerifyRange`

```go
func (s *Service) VerifyRange(ctx context.Context, req *devicelogv1.VerifyRangeRequest) (*devicelogv1.VerifyRangeResponse, error) {
	if req.DeviceId == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}
	resp, err := s.engine.VerifyRange(ctx, req.DeviceId, tsOrZero(req.From), tsOrZero(req.To))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "verify: %v", err)
	}
	return resp, nil
}
```

> **Go concept — `status` + `codes` typed errors.** A gRPC handler returns
> `(*Response, error)`. Returning a plain error would surface as
> `codes.Unknown`; instead `status.Error(codes.InvalidArgument, …)` /
> `status.Errorf(codes.Internal, "verify: %v", err)` attach a **typed status
> code** the client can branch on (the client in [13 — cli-tools](13-cli-tools.md)
> reads these). `%v` formats the wrapped error into the message. This is the gRPC
> analogue of HTTP status codes.

> **Go concept — validate at the edge.** Input checks live in the handler
> (`device_id` required), before touching the engine — fail fast with a precise
> code. `ExportAuditReport` similarly returns `codes.NotFound` when a trace has
> no entries in the window.

`tsOrZero` is a tiny helper converting a possibly-nil protobuf timestamp to a Go
`time.Time` (nil → the zero time), so callers can omit `from`/`to`.

### 2.4 A streaming handler — `Query`

```go
func (s *Service) Query(req *devicelogv1.QueryRequest, stream devicelogv1.LogService_QueryServer) error {
	entries, err := s.engine.Query(stream.Context(), query.Filter{
		Devices: req.DeviceIds, From: tsOrZero(req.From), To: tsOrZero(req.To),
		MinSeverity: req.MinSeverity, Subsystem: req.Subsystem,
		TraceID: req.TraceId, Limit: int(req.Limit),
	})
	if err != nil {
		return status.Errorf(codes.Internal, "query: %v", err)
	}
	for _, e := range entries {
		if err := stream.Send(e); err != nil {
			return err
		}
	}
	return nil
}
```

> **Go concept — server streaming.** A server-streaming RPC has a different
> handler shape: no return message, just a `stream` you push zero-or-more
> messages onto with `stream.Send(e)`, returning `nil` when done (the client
> sees `io.EOF`). `stream.Context()` is the **per-RPC context** — cancelled if
> the client hangs up — which we pass to the engine so a query stops when the
> caller leaves. Translating the protobuf request into the engine's `query.Filter`
> ([10](10-query.md)) is the adapter step.

### 2.5 `Tail` — streaming a live subscription

```go
func (s *Service) Tail(req *devicelogv1.TailRequest, stream devicelogv1.LogService_TailServer) error {
	id, ch := s.hub.Subscribe(256)
	defer s.hub.Unsubscribe(id)
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			if !tailMatch(e, req) {
				continue
			}
			if err := stream.Send(e); err != nil {
				return err
			}
		}
	}
}
```

> **Go concept — `select` over a context and a channel.** `Tail` subscribes to
> the hub ([09 — ingest](09-ingest.md)) and loops, using `select` to wait on
> **whichever happens first**: the client disconnecting (`<-stream.Context().Done()`)
> or a new entry arriving (`<-ch`). The `e, ok := <-ch` **comma-ok receive**
> detects a closed channel (`ok == false`). This is the reader side of the
> producer/consumer channel you built in the hub — the two components meet here.

> **Go concept — `defer` for guaranteed cleanup.** `defer s.hub.Unsubscribe(id)`
> runs no matter how the loop exits (client gone, channel closed, send error),
> so a dropped tail never leaks a hub subscription. RAII-without-destructors,
> exactly as in earlier docs.

### 2.6 The interceptors themselves

```go
func authorize(ctx context.Context, sessions *license.Manager) error {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization metadata")
	}
	token := strings.TrimPrefix(vals[0], "Bearer ")
	if _, err := sessions.Authorize(ctx, token, "", license.FeatureQuery); err != nil {
		return status.Errorf(codes.Unauthenticated, "license rejected: %v", err)
	}
	return nil
}

func unaryAuth(sessions *license.Manager) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := authorize(ctx, sessions); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}
```

> **Go concept — metadata rides in the context.** gRPC request headers arrive as
> **metadata** attached to the incoming `context.Context`.
> `metadata.FromIncomingContext(ctx)` extracts them; `md.Get("authorization")`
> reads the header we care about. Context is not just cancellation — it is also
> the request-scoped key/value carrier. `strings.TrimPrefix(v, "Bearer ")`
> strips the scheme to get the raw license token, which is fed to
> `Manager.Authorize(..., FeatureQuery)` — the **query** feature, separating
> readers from the writers the broker authorizes with `ingest`.

> **Go concept — a closure that returns an interceptor.** `unaryAuth(sessions)`
> is a *factory*: it captures `sessions` and returns the actual interceptor
> function. The returned `func(ctx, req, info, handler)` decides whether to call
> `handler` (proceed) or short-circuit with an error (reject). This "function
> that returns a function closing over dependencies" is how you parameterize
> middleware without globals. `any` is Go's `interface{}` alias — the request is
> type-erased at this layer.

```go
func unaryRecover(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in unary handler", "method", info.FullMethod, "panic", r)
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}
```

> **Go concept — `panic`/`recover` with named returns.** A `panic` unwinds the
> stack like a C++ exception, but Go reserves it for *programmer errors*, not
> control flow. `recover()` (only meaningful inside a `defer`) stops the unwind.
> The trick here is the **named return values** `(resp any, err error)`: the
> deferred closure can *assign* `err` after the panic, so a crashing handler
> becomes a clean `codes.Internal` response instead of taking the whole server
> down. This is the one place devlogd uses `recover` — a safety net at the
> boundary, not everyday error handling (which is always `if err != nil`).

`unaryMetrics`/`streamMetrics` similarly wrap the handler and record a
per-method, per-status-code counter. The `stream*` variants mirror the unary
ones but operate on `grpc.ServerStream` (using `ss.Context()` for metadata).

---

## 3. Deep dives

### 3.1 The interceptor chain as a uniform contract

The order `recover → authorize → meter` is deliberate:

- **recover** is outermost so it catches panics in *any* later stage, including
  a bug in `authorize` itself.
- **authorize** runs before the handler so an unauthenticated call never reaches
  business logic.
- **meter** wraps the handler to time/count the actual work.

Because `NewServer` installs this chain once, adding a sixth RPC tomorrow
inherits panic-safety, license enforcement, and metrics for free — you *cannot*
ship an unauthenticated endpoint by accident. That is the whole value of
middleware: cross-cutting concerns implemented once and applied structurally.
The MQTT hooks ([11](11-broker.md)) give the write plane the same guarantee.

### 3.2 Streaming and context lifetime

Both streaming RPCs are governed by `stream.Context()`. For `Query`, passing it
to the engine means a client that disconnects mid-query stops the server-side
work. For `Tail`, the `select` on `<-stream.Context().Done()` is what ends an
otherwise-infinite loop when the client leaves — and the `defer Unsubscribe`
cleans up the hub slot. Context is the thread that ties client lifetime to
server-side resource cleanup; internalize the pattern
`select { case <-ctx.Done(): return; case v := <-ch: … }` — it recurs in the
archiver ([08](08-archive.md)), the hub consumer, and here.

---

## 4. Idioms & gotchas

- **Embed `Unimplemented…Server`** in every gRPC service for forward
  compatibility; without it, a new RPC breaks the build.
- **Return `status.Error(code, …)`, not bare errors**, so clients get a
  meaningful `codes.*`.
- **Read auth/headers from the context via `metadata`** — request-scoped data
  lives on the context, not on a global.
- **Interceptor = a closure returning a closure** capturing dependencies; install
  the chain once at the server.
- **`recover()` only works inside a `defer`,** and needs **named returns** to
  convert a panic into an error result.
- **Wire every streaming loop to `stream.Context().Done()`** and `defer` any
  per-RPC cleanup, or you leak goroutines/subscriptions on client disconnect.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why does `Service` embed `UnimplementedLogServiceServer`? What
   breaks if you remove it and later add an RPC to the proto?
2. **Recall.** Where does the license token come from, and which feature must it
   grant? Contrast with the broker's requirement in [11](11-broker.md).
3. **Apply.** Add a `unaryLogging` interceptor that logs method + duration.
   Where in the chain should it sit relative to `recover` and why?
4. **Apply.** Make `Tail` also stop after a client-supplied max-count. Which
   `select` branch and counter do you add, and how do you avoid blocking on
   `Send`?
5. **Hero.** Write a test (see `service_test.go`) that drives `Query` with a
   mock stream and asserts filtering, then one that calls `authorize` with
   missing and malformed metadata and asserts `codes.Unauthenticated`.

---

## 6. Recap & next

You can now expose Go business logic over gRPC: TLS credentials, an interceptor
chain that enforces panic-safety and licensing uniformly, request metadata read
from the context, typed status codes, and server-streaming handlers whose
lifetime is bound to the client's context. Combined with [11 — broker](11-broker.md),
both network planes are covered.

**Next:** [13 — cli-tools](13-cli-tools.md), where you build the *client* side —
subcommand CLIs with the `flag` package, a bearer credential that refuses
plaintext, and consuming a server stream with a `Recv()` loop.
