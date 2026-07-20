# Go Concepts Index

A one-line definition of every Go language feature used in this codebase.
Each row points to the [`components/`](components/README.md) deep-dive that
teaches it in full — definition, syntax, the *why*, a C++ contrast, and the
gotchas — against the real code that ships here. Use this table for a fast
reminder; use the linked doc when you need the full treatment.

| Concept | In one line, for a C++ engineer | Taught in depth |
| --- | --- | --- |
| Modules & packages | `go.mod` is the whole project + lockfile; a package is a directory, imported by module-relative path; there are no header files | [01 · proto-contract](components/01-proto-contract.md) |
| Visibility by capitalization | An uppercase identifier is exported from its package; lowercase is package-private — no `public`/`private` keywords | [02 · config](components/02-config.md) |
| Zero values | Every type has a usable default (`0`, `""`, `nil`, an unlocked mutex) — there is no uninitialized memory | [02 · config](components/02-config.md) |
| Structs, methods, receivers | Methods are free functions with an explicit receiver (`func (s *Store) F()`); pointer receivers mutate, value receivers copy | [02 · config](components/02-config.md), [07 · cold](components/07-cold.md) |
| Interfaces (implicit satisfaction) | An interface is a method set; any type that has those methods satisfies it automatically — compile-time duck typing, no `implements` | [07 · cold](components/07-cold.md) |
| Slices & maps | A slice is a `{ptr, len, cap}` view (like `std::span`, but grows via `append`); maps are unordered hash maps | [10 · query](components/10-query.md) |
| Pointers & garbage collection | `*T`/`&x` exist, `p.f` auto-derefs, there's no pointer arithmetic and no `delete` — memory is GC'd, safe to return `&local` | [04 · sign](components/04-sign.md) |
| Errors as values | No exceptions for expected failures — `error` is a normal return value, wrapped with `fmt.Errorf("...: %w", err)` | [02 · config](components/02-config.md), [06 · hot](components/06-hot.md) |
| `defer` | Schedules a call for function return, LIFO order — RAII without destructors, but per-function, not per-scope | [07 · cold](components/07-cold.md), [11 · broker](components/11-broker.md) |
| Goroutines, channels, `select` | `go f()` starts a runtime-scheduled lightweight thread; channels are typed, thread-safe queues; `select` waits on several at once | [09 · ingest](components/09-ingest.md), [14 · composition-root](components/14-composition-root.md) |
| `context.Context` | Explicit, propagated cancellation and deadlines — cancel the root and every blocking call downstream unwinds | [08 · archive](components/08-archive.md), [14 · composition-root](components/14-composition-root.md) |
| Multiple return values | Functions return tuples natively (`data, err := f()`) — no out-params, no `std::pair` | [07 · cold](components/07-cold.md), [09 · ingest](components/09-ingest.md) |
| Struct tags | Backtick metadata read via reflection by encoders (`` `json:"..."` ``, `` `yaml:"..."` ``) — declarative serialization | [02 · config](components/02-config.md), [05 · license](components/05-license.md) |
| Embedding | Placing a type inside a struct promotes its methods — composition that *looks* like inheritance but has no virtual dispatch | [11 · broker](components/11-broker.md), [12 · grpcapi](components/12-grpcapi.md) |
| Closures | Functions are first-class values that capture their enclosing scope — used here as accumulators and iteration callbacks | [10 · query](components/10-query.md) |
| Generics | Type parameters exist but are used sparingly — reached for only where the standard library already provides them | [07 · cold](components/07-cold.md), [05 · license](components/05-license.md) |
| Testing | `_test.go` + `func TestXxx(t *testing.T)`, no framework required — `t.Helper`, `t.Cleanup`, fakes over mocks | [`TESTING.md`](TESTING.md) |
| The toolchain | `go build` / `test` / `vet` / `fmt` / `mod tidy` — the exact commands and conventions this repo expects | [`../CONTRIBUTING.md`](../CONTRIBUTING.md) |
| Protobuf code generation | `buf generate` turns `api/proto/**.proto` into committed `api/gen/**.pb.go` — one schema, every language | [01 · proto-contract](components/01-proto-contract.md) |

See [`components/README.md`](components/README.md) for the full learning-path
order — each doc lists its own prerequisites and builds on earlier ones.
