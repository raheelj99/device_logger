# Component Deep-Dives — Go from Zero to Hero

One fully-worked document per component of devlogd. Each one teaches **the Go
language through the real code that ships in this repo** — every keyword,
type, idiom, and standard-library call is explained from first principles, with
contrasts to C++ (the language you already know) and the *why* behind each
choice.

Read [`../COMPONENTS.md`](../COMPONENTS.md) first for the 10,000-ft map, then
work these in order. Each doc lists its prerequisites and the concepts it adds,
so the sequence below is a genuine learning path: earlier docs teach the
fundamentals (types, methods, interfaces, errors), later docs assume them and
layer on concurrency, streaming, and whole-system wiring.

## The learning path

| # | Document | Component | New Go territory you conquer |
| --- | --- | --- | --- |
| 01 | [proto-contract.md](01-proto-contract.md) | `api/proto`, `api/gen` | packages, modules, code generation, proto3 ↔ Go type mapping, enums, generated getters |
| 02 | [config.md](02-config.md) | `internal/config` | structs, struct tags, methods & receivers, interfaces (implicit satisfaction), maps, pointers, error wrapping, zero values |
| 03 | [telemetry.md](03-telemetry.md) | `internal/telemetry` | `slog`, function values/closures, the `net/http` server, graceful shutdown, dependency injection over globals |
| 04 | [sign.md](04-sign.md) | `internal/sign` | byte slices, `crypto/*`, value vs pointer semantics, PEM/DER, deterministic marshaling, small focused interfaces |
| 05 | [license.md](05-license.md) | `internal/license` | `encoding/json`, `time`, `slices`, `sync.Mutex`, `http.Client`, embedding, sentinel design |
| 06 | [hot.md](06-hot.md) | `internal/hot` | Redis client, pipelines/transactions, `errors.Is` & sentinels, callbacks, string/number formatting |
| 07 | [cold.md](07-cold.md) | `internal/cold` | `io.Reader`/`io.Writer`, `bufio`, `encoding/binary` (varint), compression, ports & adapters, `defer` |
| 08 | [archive.md](08-archive.md) | `internal/archive` | long-running loops, `context` cancellation, timers/backoff, at-least-once semantics, graceful drain |
| 09 | [ingest.md](09-ingest.md) | `internal/ingest` | goroutines, `sync.Mutex` critical sections, channels & `select`, non-blocking sends, ULIDs |
| 10 | [query.md](10-query.md) | `internal/query` | slices & `sort`, closures as accumulators, dedupe algorithms, `bytes.Equal`, hashing pipelines |
| 11 | [broker.md](11-broker.md) | `internal/broker` | embedding third-party interfaces (hooks), method sets, TLS wiring, adapter thinking |
| 12 | [grpcapi.md](12-grpcapi.md) | `internal/grpcapi` | gRPC server & streaming, interceptor chains (middleware), `context` metadata, `status`/`codes` |
| 13 | [cli-tools.md](13-cli-tools.md) | `cmd/lictl`, `cmd/logctl`, `cmd/licensed`, `tools/gencerts` | `func main`, `flag`, `os.Args`, subcommands, `PerRPCCredentials`, exit codes |
| 14 | [composition-root.md](14-composition-root.md) | `cmd/devlogd` | `errgroup`, `signal.NotifyContext`, structured concurrency, assembling the whole system, `atomic` |

## How each document is structured

Every doc follows the same shape so you always know where to look:

1. **Orientation** — what the component does and where it sits.
2. **What you'll master** — the Go and domain concepts it teaches.
3. **Guided code walkthrough** — the real source, in chunks, with a
   **`Go concept`** callout the first time each feature appears (definition →
   syntax → why → C++ contrast → gotchas).
4. **Deep dives** — extended treatment of the component's signature concepts.
5. **Idioms & gotchas** — the patterns and pitfalls demonstrated.
6. **Exercises** — graded tasks (recall → apply → extend) to reach "hero".
7. **Recap & next.**

## Companion docs

- [`../GO_CONCEPTS.md`](../GO_CONCEPTS.md) — a one-line-per-concept index, if
  you just need a fast reminder of what something means before jumping back
  into a doc below.
- [`../COMPONENTS.md`](../COMPONENTS.md) — the architectural reference.
- [`../TESTING.md`](../TESTING.md) — how each component is verified.
