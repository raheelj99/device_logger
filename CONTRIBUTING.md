# Contributing

Formatting conventions this codebase follows, and how to extend it without
quietly breaking one of its cross-language guarantees.

There is no CI in this repo and no linter config for any language — every
convention below is precedent, enforced by review, not by tooling. Run the
commands in [§ Before you commit](#before-you-commit) yourself.

## Formatting by language

### Go (primary language, `go 1.25`)

- **Formatting is `gofmt`, full stop.** No style debates. Run `gofmt -l .`
  before committing — it should print nothing. `go vet ./...` must be clean.
- **Imports** are grouped in three blocks, blank-line separated: stdlib,
  third-party, then `devlog/internal/...`. `goimports` will do this for you.
- **Package doc comment**: exactly one `// Package x ...` comment per package,
  a 1–2 sentence paragraph naming the package's role plus its key design
  rationale (e.g. `internal/archive/archiver.go:1-4`). No `doc.go` files.
- **Comments are "why," not "what."** Trivial functions get zero comments.
  A comment appears only where the reasoning isn't obvious from the code
  itself — e.g. why a mutex spans a whole critical section, why a batch
  retries instead of reading more. If removing a comment wouldn't confuse a
  reader, don't add it.
- **Errors are values, wrapped, never typed.** `fmt.Errorf("...: %w", err)` is
  the one wrapping idiom used everywhere. There are no sentinel error vars
  (`errors.New`) and no custom `Error()` types anywhere in this repo —
  `errors.Is` is used only against third-party/stdlib sentinels (`redis.Nil`,
  `http.ErrServerClosed`, `io.EOF`). Keep it that way: reach for `%w` wrapping,
  not a new error type, unless you have a concrete caller that needs
  `errors.As`. At a gRPC boundary, convert with
  `status.Errorf(codes.X, "...: %v", err)` (`%v`, not `%w` — a status error
  isn't meant to be unwrapped further).
- **`cmd/*/main.go` shape**: `main()` parses flags and calls `run(...) error`;
  on failure, `fmt.Fprintln(os.Stderr, "<progname>:", err); os.Exit(1)`. Follow
  this shape for any new binary.
- **Naming**: package names are short, lowercase, no underscores, and match
  the directory. Constructors are `New(...)` when a package has one obvious
  primary type, `NewX(...)` otherwise. Unexported helpers are verb-first
  lowerCamelCase. Interfaces are small and defined by the consumer, not the
  implementor (the one example in this repo is `cold.ObjectStore`) — Go's
  usual single-method `-er` naming isn't followed for it, since it's a
  ports-and-adapters boundary, not a behavioral abstraction.
- **Logging** is `log/slog`, JSON handler, always structured key-value pairs
  with short lowercase keys and `"err"` for error values — never
  `fmt.Sprintf` into a log message.
- **Tests**: stdlib `testing` only (no testify). One `func TestXxx` per
  scenario, named for the behavior it proves (`TestIngestRejectsIdentityMismatch`),
  not table-driven. Use `t.Helper()` in shared setup and a `newTestX(t)`
  constructor at the top of the file. Prefer fakes/in-memory implementations
  (`miniredis`, an in-memory `ObjectStore`) over mocking framework or real
  infrastructure. Colocate `x_test.go` next to `x.go` — never a package-wide
  test file.
- **File organization**: one file per concern within a package (e.g.
  `sign/keys.go` for PEM/keypair I/O vs `sign.go` for signing logic), not one
  file per package.

### Protobuf (`api/proto/`, `clients/proto/` — kept as parallel copies)

- snake_case fields, `PascalCase` messages/services, `UPPER_SNAKE` enum values
  prefixed by the enum name (`SEVERITY_*`), K&R braces on the same line as the
  message/enum name, a prose comment above each message and a trailing `//`
  comment on non-obvious fields.
- `buf.yaml` pins `lint: use: [STANDARD]` with two explicit, justified
  exceptions — read the inline comments there before adding a third. `buf
  lint` only checks lint rules, **not formatting** — there's no `buf format`
  Makefile target, so run `buf format -d api/proto/devicelog/v1/*.proto`
  yourself before committing a proto change and fix anything it flags.
- `api/proto` and `clients/proto` are two copies of the same schema, not one
  shared via import — a proto change must be applied to both, by hand.

### TypeScript (`clients/nest`), Python (`clients/python`), Node (`clients/node`)

No ESLint/Prettier/Black/Ruff config exists for any of them — style is
by-hand convention only:

- 2-space indent, single quotes, semicolons (TS); 4-space indent, double
  quotes, type hints on public signatures, `from __future__ import
  annotations` (Python); same 2-space/single-quote feel in plain Node despite
  no shared tooling with the TS client.
- Every source file opens with a short `//`/`#` purpose comment before
  imports — the same convention, and the same "why not what" density, as the
  Go package doc comments.
- Errors are raised as builtin/stdlib exception types (`ValueError`,
  `throw new Error(...)`) — no custom exception classes, matching Go's
  no-custom-error-types convention.
- Test runners are intentionally per-ecosystem and not reconciled: Jest for
  `clients/nest`, `node --test` for `clients/node`, `pytest` for
  `clients/python`. Match whichever your client already uses; don't introduce
  a fourth.

## Before you commit

```bash
# Go
gofmt -l .                 # must print nothing
go vet ./...
go test -race ./...

# Proto, if you touched api/proto or clients/proto
buf format -d api/proto/devicelog/v1/*.proto clients/proto/devicelog/v1/*.proto
buf lint

# Any client you touched
(cd clients/node && npm test)
(cd clients/nest && npm test)
(cd clients/python && pytest -m "not integration")
```

## How to extend the codebase

### Changing the proto contract (`api/proto/devicelog/v1/*.proto`)

1. Only *add* fields, with fresh numbers. Never renumber or retype an
   existing field; `reserve` the numbers of anything removed.
2. Apply the same edit to `clients/proto/devicelog/v1/*.proto` — it is not
   generated from `api/proto`, it's a hand-kept parallel copy.
3. Regenerate: `buf generate` (Go, into `api/gen/`, committed); for the Python
   client, `clients/python/scripts/gen_proto.sh`. The Node/Nest clients
   compile `.proto` directly at runtime via `protobufjs`, nothing to
   regenerate.
4. Never make a client-settable field one the server should own. `entry_id`,
   `ingest_time`, and `audit` are filled by `internal/ingest` regardless of
   what a producer sends — any new server-owned field should follow the same
   rule and be structurally unsettable in every client's builder (tested).
5. If the change is wire-visible to the C++ integrator, update the
   integration contract table in `docs/MANUAL.md`.

### Adding a new `internal/` component

1. One directory, one package, one package doc comment stating its
   responsibility and the one design decision worth flagging.
2. Take dependencies as constructor parameters and wire the component in
   `cmd/devlogd/main.go` (the composition root) — no package-level globals.
3. If it talks to an external system (a new storage backend, a new broker),
   define a small interface from the consumer's side (see `cold.ObjectStore`)
   so tests can fake it instead of standing up real infrastructure.
4. Colocate `x_test.go`; prefer `miniredis`/in-memory fakes over mocks or live
   dependencies, following the existing test conventions above.
5. Add a summary section to `docs/COMPONENTS.md` (responsibility, public
   surface, concepts, failure behavior — follow the shape of an existing
   section). Only add a new `docs/components/NN-*.md` deep-dive if the
   component teaches a genuinely new Go concept worth a full walkthrough —
   see `docs/components/README.md`'s "how each document is structured" — a
   small component doesn't need one.

### Adding a new reference client language

1. Mirror `clients/node`'s shape: `config` / `entry` (pure builders) /
   `publisher` (MQTT/TLS) / `observer` (gRPC/TLS) / a CLI or service entry
   point.
2. Compile `clients/proto` directly for that language's protobuf/gRPC
   tooling — never hand-write the wire structs.
3. Make the entry builder structurally incapable of setting `entry_id`,
   `ingest_time`, or `audit`.
4. Ship a protobuf round-trip unit test (encode/decode against the shared
   `.proto`, assert field numbers/types) and an `e2e` command that publishes
   then reads back a job, matching the other clients.
5. Add a row to `clients/README.md`'s table and write a client-local
   `README.md` for install/run instructions — don't duplicate the shared
   contract description already in `clients/README.md`.

### Adding or changing documentation

Each doc has one job. Before writing, place new content by *what kind of
question it answers*, not by which file happens to be open:

| If you're explaining... | It belongs in |
| --- | --- |
| How to run, configure, or operate devlogd | `docs/MANUAL.md` |
| Code formatting or how to extend the codebase | this file |
| *Why* the architecture, deployment, or clients are shaped this way | `docs/DESIGN.md` |
| What a component's responsibility/public surface is, at a glance | `docs/COMPONENTS.md` |
| A Go language concept, taught from first principles against real code | `docs/components/NN-*.md` — the **only** place this belongs |
| A one-line reminder of what a Go concept means | `docs/GO_CONCEPTS.md` (index only — no prose explanation, just a link) |
| What's tested, with what tooling, and how to run it | `docs/TESTING.md` |

If you catch yourself re-explaining something a linked doc already covers,
link it instead of restating it — that's the rule this pass of cleanup
exists to enforce.
