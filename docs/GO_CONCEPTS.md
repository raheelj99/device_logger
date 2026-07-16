# Go for a C++ Engineer — a tour through this codebase

Every concept below is anchored to a real file in this repository, so you can
read the theory and immediately see it used in production-grade code. Read this
top to bottom once, then use it as a reference while reading the source.

---

## 1. Modules, packages, imports

A **module** is the unit of versioning and dependency management — think "the
whole CMake project + its lockfile". It's declared in `go.mod`:

```go
module devlog        // the import-path prefix for everything in this repo
go 1.25.0
require ( ... )      // direct dependencies; go.sum pins cryptographic hashes
```

A **package** is the unit of compilation and visibility — roughly a C++
namespace + translation-unit group. One directory = one package. Every `.go`
file in `internal/hot/` starts with `package hot`, and other code imports it as:

```go
import "devlog/internal/hot"    // path = module name + directory
```

Two conventions with teeth:

- **`internal/`** is compiler-enforced privacy: packages under `internal/` can
  only be imported by code inside this module. Our whole implementation lives
  there; only `api/` (the proto contract) is importable from outside.
- **`cmd/<name>/`** holds `package main` binaries. Each is a thin composition
  root (`cmd/devlogd/main.go` wires everything; the logic lives in `internal/`).

There are **no header files**: a package's exported identifiers *are* its
interface, extracted by the compiler.

## 2. Visibility by capitalization

There is no `public:`/`private:`. An identifier starting with an **uppercase**
letter is exported from its package; lowercase is package-private.

`internal/hot/store.go`:

```go
func (s *Store) AppendWithChain(...) error   // exported — API of the package
func streamKey(device string) string          // unexported helper
```

This applies to struct fields too — which matters for serialization (§13).

## 3. Variables, type inference, zero values

```go
var retention time.Duration        // declaration; initialized to its ZERO VALUE
retention := 24 * time.Hour        // := declares + infers type (function scope only)
```

Every type has a well-defined zero value (`0`, `""`, `nil`, zeroed struct).
There is no uninitialized memory — code exploits this: `config.Load()` in
`internal/config/config.go` starts from `defaults()` and lets YAML overwrite,
and a `sync.Mutex`'s zero value is an unlocked, usable mutex (no constructor).

## 4. Structs, methods, receivers

Methods are declared *outside* the struct, with an explicit receiver — like a
free function taking `this` as the first parameter:

```go
type Store struct {                         // internal/hot/store.go
    rdb       *redis.Client
    retention time.Duration
}

func (s *Store) Trim(ctx context.Context) error { ... }
```

- `(s *Store)` — **pointer receiver**: can mutate, no copy. Like `T::f()`.
- `(m Manifest)` — **value receiver** (`internal/cold/segment.go`): read-only
  by copy. Like `f() const` on a small value type.

Rule of thumb used throughout this repo: pointer receivers for anything with
identity or mutexes, value receivers for small immutable values.

Constructors are plain functions named `New*` returning a pointer:
`hot.New(...)`, `ingest.NewPipeline(...)`. No overloading, no default args —
Go's answer is explicit parameters or an options struct (`redis.Options`).

## 5. Interfaces — implicit satisfaction

The single biggest mental shift from C++. An interface is a method set, and
**any type that has those methods satisfies it automatically** — no
inheritance declaration, no vtable annotation at the definition site.

`internal/cold/store.go`:

```go
type ObjectStore interface {
    Put(ctx context.Context, key string, data []byte, contentType string) error
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    List(ctx context.Context, prefix string) ([]string, error)
}
```

`MinioStore` (same file) satisfies it, and so does `fakeStore` in
`internal/cold/cold_test.go` — which never mentions `ObjectStore` at all. This
is duck typing checked at compile time, and it's what makes the storage layer
testable without MinIO (the "ports and adapters" pattern, see DESIGN.md).

The idiom: **accept interfaces, return concrete types**, and keep interfaces
small (1–3 methods) and defined *by the consumer*, not the implementor.

## 6. Slices and maps

```go
var out []*devicelogv1.LogEntry        // slice: like span-over-vector, nil is a valid empty slice
out = append(out, e)                   // may reallocate — always reassign
seen := map[string]struct{}{}          // map literal; struct{} = zero-byte "set member"
if _, dup := seen[id]; dup { ... }     // "comma ok": lookup + existence in one step
```

A slice is a `{ptr, len, cap}` view over a backing array — copying one is O(1)
and both copies see the same elements (watch out, like `std::span`). `append`
is the only growth mechanism. Maps are unordered hash maps; iteration order is
deliberately randomized. See `seen` used for query-time deduplication in
`internal/query/engine.go`.

`for` is the only loop, and `range` is the iterator protocol:

```go
for device, ids := range a.acks { ... }   // internal/archive/archiver.go
```

## 7. Pointers, no pointer arithmetic, garbage collection

`*T` and `&x` exist; `p->f` is spelled `p.f` (auto-deref); there is **no**
pointer arithmetic and no `delete` — memory is garbage-collected. It is safe
and idiomatic to return pointers to local variables (`return &Signer{...}`) —
the compiler moves them to the heap ("escape analysis"). Lifetimes are simply
not your problem; ownership discipline is replaced by *concurrency* discipline
(§10).

## 8. Error handling — values, not exceptions

Go has no exceptions for expected failures. Functions return an `error` as the
last result, and callers must decide at every call site:

```go
cfg, err := config.Load(cfgPath)      // cmd/devlogd/main.go
if err != nil {
    return err
}
```

Errors are wrapped with context using `fmt.Errorf` and `%w`, forming a chain
that `errors.Is`/`errors.As` can inspect — the structured equivalent of nested
exception types:

```go
return fmt.Errorf("redis unreachable at %s: %w", cfg.Redis.Addr, err)
...
if errors.Is(err, redis.Nil) { return nil, nil }   // internal/hot/store.go
```

`panic` exists but is reserved for programmer bugs; long-running servers
convert stray panics into errors at a boundary — see the `recover()` in
`unaryRecover`, `internal/grpcapi/server.go`. That's the moral equivalent of a
top-level `catch(...)` around each request.

The discipline "an error is either handled or returned, never silently
swallowed" is the backbone of the reliability story here.

## 9. `defer` — RAII without destructors

`defer f()` schedules `f` to run when the current function returns, in LIFO
order — Go's replacement for destructors/scope guards:

```go
p.mu.Lock()                    // some paths in this repo lock/unlock manually
defer cancel()                 // internal/broker/hooks.go — always cancels the context
defer rc.Close()               // internal/cold/store.go — file/stream cleanup
```

Unlike RAII it's *per function*, not per scope, and it's explicit — forgetting
a `defer` is possible, which is why `go vet` and code review matter (§16).

## 10. Goroutines, channels, `select`

A **goroutine** is a runtime-scheduled lightweight thread (KBs of stack,
multiplexed over OS threads). Start one with `go f()`. This service runs its
MQTT broker, gRPC server, archiver, janitor, and HTTP server as concurrent
goroutines from `cmd/devlogd/main.go`.

A **channel** is a typed, thread-safe queue used for communication *and*
synchronization — the idiom is "share memory by communicating":

```go
ch := make(chan *devicelogv1.LogEntry, buffer)   // internal/ingest/hub.go
select {                                          // wait on several channels at once
case ch <- e:                                     // delivered
default:                                          // would block → drop (backpressure policy)
}
```

`internal/ingest/hub.go` is a complete, minimal pub/sub built this way; the
non-blocking `select` is a deliberate policy: a slow gRPC tail subscriber
drops live entries instead of stalling ingestion.

Classic mutual exclusion still exists — `sync.Mutex` guards the hash-chain
critical section in `internal/ingest/pipeline.go`, because chain linearity
demands read-sign-append be atomic per device.

**errgroup** (`golang.org/x/sync/errgroup`, used in `cmd/devlogd/main.go`) is
structured concurrency: run N goroutines, wait for all, first error cancels
the shared context — the pattern that gives devlogd one-line graceful
shutdown semantics.

## 11. `context.Context` — cancellation as a value

Every potentially blocking call in Go takes a `ctx context.Context` first
parameter. It carries cancellation, deadlines, and request-scoped values down
the call tree. Cancel the root and everything downstream unwinds:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
```

That single line in `cmd/devlogd/main.go` is the entire Ctrl-C story: the OS
signal cancels `ctx`, the errgroup propagates it, the broker/archiver/gRPC
server all observe `ctx.Done()` and drain. Compare `Archiver.Run` — its loop
checks `ctx.Err()` and does a final flush on the way out.

## 12. Multiple return values

Functions return tuples natively — no out-params or `std::pair`:

```go
data, m, err := EncodeSegment(entries)         // internal/cold/segment.go
id, ch := h.Subscribe(256)                     // internal/ingest/hub.go
```

The `_` blank identifier discards a value: `md, _ := metadata.FromIncomingContext(ctx)`.

## 13. Struct tags — declarative serialization

The backtick strings after struct fields are **tags**: metadata read at
runtime via reflection by encoders:

```go
type License struct {                          // internal/license/license.go
    ID      string   `json:"lic_id"`
    Subject string   `json:"subject"`
    ...
}
HotRetention Duration `yaml:"hot_retention"`   // internal/config/config.go
```

This replaces hand-written serialization code. Note `config.Duration`: a named
type with a custom `UnmarshalYAML` method so YAML can say `24h` — Go's version
of a user-defined conversion.

## 14. Embedding — composition over inheritance

Go has no class inheritance. **Embedding** places a type inside a struct and
promotes its methods:

```go
type authHook struct {                         // internal/broker/hooks.go
    mqtt.HookBase                              // embedded: inherits no-op implementations
    sessions *license.Manager
    ...
}
func (h *authHook) OnConnectAuthenticate(...) bool { ... }  // override just what you need
```

`mqtt.HookBase` provides default implementations of the ~30-method Hook
interface; we override four. Same pattern with
`devicelogv1.UnimplementedLogServiceServer` in `internal/grpcapi/service.go` —
embedding it keeps the service forward-compatible when RPCs are added to the
proto. It looks like inheritance, but there's no virtual dispatch through the
base: it is pure composition + name promotion.

## 15. Closures and function values

Functions are first-class. The query engine builds its collector as a closure
capturing `seen`, `out`, and the filter (`internal/query/engine.go`):

```go
collect := func(le *devicelogv1.LogEntry) bool {
    if _, dup := seen[le.EntryId]; dup { return true }
    ...
}
```

Callbacks like `hot.Store.Range(ctx, device, from, to, fn)` use this shape
(`func(*LogEntry) bool` — return false to stop) instead of iterator objects.

## 16. Generics — used sparingly

Go has type parameters (`func Min[T cmp.Ordered](a, b T) T`), but idiomatic Go
reaches for them far less than C++ templates. In this codebase generics appear
only where the standard library provides them (`min`, `max` builtins in
`internal/cold/segment.go`; `slices.Contains` in `internal/license/license.go`).
Interfaces cover most polymorphism needs.

## 17. Testing — built into the toolchain

Any file ending `_test.go` is a test file; any `func TestXxx(t *testing.T)` is
a test. No framework needed — `go test ./...` runs everything.

Patterns used here (`internal/sign/sign_test.go`, `internal/hot/store_test.go`,
`internal/cold/cold_test.go`, `internal/license/license_test.go`):

- **Helpers**: `t.Helper()` marks a function so failures blame the caller.
- **Cleanup**: `t.Cleanup(func(){ ... })` — deferred teardown tied to the test.
- **Fakes over mocks**: `fakeStore` implements `ObjectStore` in ~30 lines; the
  real Redis is replaced by `miniredis` (an in-process Redis implementation).
- **Behavioral assertions**: tests state facts ("tampered entry must fail
  verification") rather than checking call sequences.

## 18. The toolchain

| Command            | What it does                                              |
| ------------------ | --------------------------------------------------------- |
| `go build ./...`   | compile everything (`./...` = this dir and all below)     |
| `go test ./...`    | run all tests                                             |
| `go vet ./...`     | static analysis for real bugs                             |
| `gofmt` / `go fmt` | THE formatter — no style debates, code is canonical       |
| `go mod tidy`      | sync `go.mod`/`go.sum` with actual imports                |
| `go run ./cmd/x`   | compile + run in one step                                 |
| `go install pkg@latest` | fetch + build + install a tool binary                |

Cross-compilation is built in (`GOOS=linux GOARCH=arm64 go build` — relevant
for robots). `CGO_ENABLED=0` (see `deploy/Dockerfile`) produces a fully static
binary: the entire service is **one file** with no runtime dependencies.

## 19. Protobuf code generation

`api/proto/devicelog/v1/*.proto` is the schema; `buf generate` (config in
`buf.gen.yaml`) produces `api/gen/devicelog/v1/*.pb.go` — Go structs +
gRPC client/server stubs. The generated code is committed, so builders don't
need buf installed. The same `.proto` files generate the C++ classes for the
producer side (`examples/cpp/publisher/CMakeLists.txt`) — one contract, two
languages, zero drift.
