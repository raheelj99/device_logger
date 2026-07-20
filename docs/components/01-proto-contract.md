# Component 01 — `api/proto` & `api/gen`

> **Role:** the single schema that every producer and consumer in the system is
> generated from — the wire and storage format for a `LogEntry`, plus the
> `LogService` RPC surface. | **Source:** `api/proto/devicelog/v1/log.proto`,
> `api/proto/devicelog/v1/query.proto`, `api/gen/devicelog/v1/**` (generated),
> `buf.yaml`, `buf.gen.yaml`

**Where this sits in the journey:** this is doc 01, the very first stop, and it
has **no prerequisite**. Before you can read a line of devlogd's Go you need to
know what a *package*, a *module*, and a *generated type* are — because the
first types every other component touches (`LogEntry`, `Severity`, `Audit`) are
not hand-written Go at all. They are emitted by a code generator from a
language-neutral schema. This doc teaches how Go is organised (modules,
packages, import paths) and how a `.proto` file becomes the `*devicelogv1.LogEntry`
you'll see everywhere from doc 02 onward.

## What you'll master

- **Go:** modules, packages, and **import paths** (`go.mod`, the module path,
  how an import string resolves to a directory); the difference between a
  *package name* and an *import path*; **code generation** as a build step and
  why generated code is committed; the **proto3 → Go type mapping** —
  message→struct with **pointer semantics**, scalar→value with zero-value
  defaults, `enum`→a **defined `int32` type** with typed constants, `repeated`→
  **slice**, `map`→Go **map**, message-typed field→**pointer** (`nil` = unset),
  well-known types (`google.protobuf.Timestamp` → `*timestamppb.Timestamp`);
  **struct tags** on generated fields; **nil-safe generated getters**; and the
  wire-compatibility rules encoded in field numbers.
- **Domain:** the `LogEntry` universal record, the `Audit` tamper-evidence
  block, the `SanitizationEvent` model (NIST SP 800-88 / IEEE 2883), the five
  `LogService` RPCs, and the concept of *server-owned fields*.

---

## 1. Orientation

Every part of devlogd — the MQTT ingest path, the Redis buffer, the S3
segments, the gRPC query API, and the C++, Node, and Python clients — agrees on
exactly one definition of what a log record *is*. That definition lives in two
`.proto` files under `api/proto/devicelog/v1/`. They are not Go. They are a
**schema** written in Protocol Buffers, and a generator (`buf`, wrapping
`protoc`) turns them into real Go source under `api/gen/`.

Because there is a single schema, and every language's client is generated from
it, wire drift between a producer and a consumer is **structurally impossible**:
you cannot forget to update one side, because there is only one side to update.
This is the *schema-first* discipline. Doc 02's `config` package is the first
hand-written Go you'll read; this doc is about the code you *don't* write but
depend on completely.

---

## 2. Guided code walkthrough

### 2.1 The module — `go.mod`

```go
module devlog

go 1.25.0

require (
	// … third-party dependencies pinned to exact versions …
	google.golang.org/grpc v1.82.0
)
```

> **Go concept — modules.** A **module** is the unit of dependency management
> and versioning; it is declared by a `go.mod` file at the repository root. The
> first line, `module devlog`, sets the **module path** — the prefix under which
> every package in this repo is imported. There is no C++ analogue: it combines
> what CMake targets, `#include` search paths, and a package manager
> (`vcpkg`/`conan`) each do separately. `require` pins each dependency to an
> exact semantic version, and `go.sum` (not shown) records their cryptographic
> checksums, so a build is reproducible byte-for-byte.

### 2.2 The proto package and the `go_package` option

```proto
syntax = "proto3";

package devicelog.v1;

import "google/protobuf/timestamp.proto";

option go_package = "devlog/api/gen/devicelog/v1;devicelogv1";
```

> **Go concept — packages vs the proto package.** These are two different
> namespaces that happen to line up here. `package devicelog.v1;` is the
> **protobuf** package — the namespace clients see on the wire and in other
> languages. The `go_package` option tells the generator where the *Go* output
> goes and what to call it. Its value has two halves split by `;`: the part
> before is the **import path** (`devlog/api/gen/devicelog/v1` — the module path
> `devlog` plus the directory), and the part after is the **package name**
> (`devicelogv1`). So other Go files write `import "devlog/api/gen/devicelog/v1"`
> but refer to its contents as `devicelogv1.LogEntry`. In Go the import path and
> the package name need not match — a frequent surprise coming from C++, where a
> header's path and its contents have no such formal link at all.

> **Go concept — import path = module path + directory.** There is no include
> search list to configure. An import string is literally the module path
> followed by the path to the package's directory inside the module. `api/gen`
> exists on disk, so `devlog/api/gen/devicelog/v1` resolves with zero
> configuration. Standard-library imports (`"fmt"`, `"time"`) are the paths with
> no module prefix; third-party ones carry their own module path
> (`"google.golang.org/grpc"`).

### 2.3 Generation is a build step — `buf.gen.yaml` and `buf.yaml`

```yaml
# buf.gen.yaml
version: v2
plugins:
  - local: protoc-gen-go
    out: api/gen
    opt: paths=source_relative
  - local: protoc-gen-go-grpc
    out: api/gen
    opt: paths=source_relative
```

```yaml
# buf.yaml
version: v2
modules:
  - path: api/proto
lint:
  use:
    - STANDARD
breaking:
  use:
    - WIRE_JSON
```

Running `buf generate` feeds every `.proto` under `api/proto` through two
plugins: `protoc-gen-go` emits the message/enum types (`log.pb.go`,
`query.pb.go`) and `protoc-gen-go-grpc` emits the service stubs
(`query_grpc.pb.go`). `paths=source_relative` mirrors the proto directory tree
into `api/gen`.

> **Go concept — code generation, and why generated code is committed.** Go has
> no macros and (before generics) no templates; the idiomatic way to turn a
> declarative spec into typed code is an external generator that writes `.go`
> files. Here the source of truth is the `.proto`; the `.pb.go` files are
> derived artefacts. Yet they are **checked into git**. That is deliberate and
> very Go: `go build` never runs the generator, so anyone can clone and compile
> with only a Go toolchain — no `buf`, no `protoc`, no network. Generated files
> carry a `// Code generated … DO NOT EDIT.` header; tools recognise it and you
> must never hand-edit them (re-generation would erase your changes).

`buf.yaml` adds two guardrails that run in CI, not in `go build`: `lint`
enforces naming conventions, and `breaking` with `WIRE_JSON` fails the build if
a change would break wire *or* JSON compatibility with the previous schema — the
compatibility rules of §3.2 mechanically enforced.

### 2.4 A generated enum — `Severity`

```go
type Severity int32

const (
	Severity_SEVERITY_UNSPECIFIED Severity = 0
	Severity_SEVERITY_TRACE       Severity = 1
	Severity_SEVERITY_DEBUG       Severity = 2
	// … INFO=3, WARN=4, ERROR=5, FATAL=6 …
)

func (x Severity) String() string { /* … name lookup … */ }
```

> **Go concept — `enum` → a defined int32 type with typed constants.** Go has no
> dedicated `enum` keyword. The generator emits the C++-ish equivalent: a
> **defined type** `type Severity int32` (a distinct type over `int32`, exactly
> the newtype pattern doc 02 uses for `Duration`) plus a block of `const`
> values of that type. Because `Severity` is its own type, a function taking a
> `Severity` will not silently accept a bare `int` — you get compile-time safety
> closer to C++'s `enum class` than to a plain C `enum`. The generated
> `String()` method (satisfying `fmt.Stringer`) makes `fmt.Println(sev)` print
> `SEVERITY_INFO` rather than `3`.

> **Go concept — the zero-value convention for enums.** Notice `…_UNSPECIFIED =
> 0` is always first. proto3 gives every scalar field a **zero-value default**
> and cannot tell "set to 0" from "never set", so the schema reserves 0 for
> "unknown/unset" on purpose. A freshly created `LogEntry{}` has
> `Severity == Severity_SEVERITY_UNSPECIFIED` for free — no constructor needed.

### 2.5 A generated message — the `LogEntry` struct

```go
type LogEntry struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	EntryId       string                 `protobuf:"bytes,1,opt,name=entry_id,json=entryId,proto3" json:"entry_id,omitempty"`
	DeviceId      string                 `protobuf:"bytes,2,opt,name=device_id,json=deviceId,proto3" json:"device_id,omitempty"`
	DeviceTime    *timestamppb.Timestamp `protobuf:"bytes,3,opt,name=device_time,…"`
	IngestTime    *timestamppb.Timestamp `protobuf:"bytes,4,opt,name=ingest_time,…"`
	Severity      Severity               `protobuf:"varint,5,opt,name=severity,…,enum=devicelog.v1.Severity"`
	Subsystem     string                 `protobuf:"bytes,6,…"`
	Message       string                 `protobuf:"bytes,7,…"`
	Attributes    map[string]string      `protobuf:"bytes,8,rep,name=attributes,…"`
	Payload       []byte                 `protobuf:"bytes,9,…"`
	Seq           uint64                 `protobuf:"varint,10,…"`
	TraceId       string                 `protobuf:"bytes,11,…"`
	Sanitization  *SanitizationEvent     `protobuf:"bytes,12,…"`
	Audit         *Audit                 `protobuf:"bytes,15,…"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}
```

This one struct is the entire proto3 → Go type mapping in miniature. Read it
field by field against `log.proto`:

> **Go concept — message → struct.** Each proto `message` becomes a Go `struct`.
> The exported (capitalised) fields carry your data; the lowercase `state`,
> `unknownFields`, and `sizeCache` are runtime bookkeeping — package-private
> plumbing you never touch (see §3.2 for what `unknownFields` buys you).

> **Go concept — scalar → value type with a zero default.** `string message = 7`
> becomes `Message string`; `uint64 seq = 10` becomes `Seq uint64`. These are
> plain value fields. Their absence is indistinguishable from their zero value
> (`""`, `0`) — proto3 has no "null string". If you need "unset" to be
> distinct, you model it with a message type (a pointer), which is exactly why
> timestamps are pointers below.

> **Go concept — message field → pointer (`nil` = unset).** `DeviceTime`,
> `IngestTime`, `Sanitization`, and `Audit` are all `*T`. A message-typed field
> is a **pointer** precisely so it has a value — `nil` — that means "this
> sub-message was never set", which a value struct could not express. This is
> the pointer semantics you must internalise: `entry.Audit == nil` is the honest
> test for "no audit block yet", and dereferencing it blindly panics (see the
> getters in §2.6).

> **Go concept — well-known types → `timestamppb`.** `google.protobuf.Timestamp`
> is a *well-known type* shipped with protobuf; it maps to
> `*timestamppb.Timestamp` from `google.golang.org/protobuf/types/known/
> timestamppb`. Convert with `timestamppb.New(t)` (from `time.Time`) and
> `ts.AsTime()` (back to `time.Time`). The two clocks here — `device_time`
> (producer) and `ingest_time` (server) — are both this type.

> **Go concept — `repeated` → slice, `map` → map.** `repeated` in the request
> messages (e.g. `device_ids`) becomes a Go **slice** `[]string`; `map<string,
> string> attributes` becomes a built-in Go **map** `map[string]string`. Both
> have `nil` as their zero value, and both are safe to *read* when `nil` (ranging
> over a nil map yields nothing) — you only need to allocate before *writing*.

> **Go concept — `bytes` → `[]byte`.** `bytes payload = 9` becomes `[]byte` — a
> byte slice, the universal currency for opaque binary in Go (doc 04's crypto
> code lives in `[]byte`). The `Audit` hashes and signatures are `[]byte` too.

> **Go concept — struct tags carry the wire spec.** The backtick metadata —
> `` `protobuf:"bytes,1,opt,name=entry_id,…"` `` — is a **struct tag**, the same
> reflection-read mechanism doc 02 uses for YAML. The `protobuf:"…,1,…"` records
> the field's **wire number**; the `json:"entry_id,omitempty"` controls JSON
> encoding. This is how the runtime (de)serialises without any per-field code:
> it reads these tags via reflection.

### 2.6 Generated getters — nil-safe accessors

```go
func (x *LogEntry) GetSeverity() Severity {
	if x != nil {
		return x.Severity
	}
	return Severity_SEVERITY_UNSPECIFIED
}

func (x *LogEntry) GetAudit() *Audit {
	if x != nil {
		return x.Audit
	}
	return nil
}
```

> **Go concept — nil-safe getters.** For every field the generator emits a
> `GetX()` method with a **pointer receiver** that first checks `x != nil`.
> Calling a method on a nil pointer is legal in Go *as long as the method does
> not dereference it* — so `var e *LogEntry; e.GetSeverity()` safely returns the
> zero value instead of panicking. This lets you chain through optional
> sub-messages: `entry.GetSanitization().GetVerification().GetPassed()` returns
> `false` cleanly even if `Sanitization` is nil, because each getter absorbs the
> nil. Prefer `GetX()` over direct field access exactly when a link in the chain
> might be unset — it is the idiomatic guard against nil-pointer panics on proto
> messages.

### 2.7 The service — `LogService` and its generated interface

```proto
service LogService {
  rpc Query(QueryRequest) returns (stream LogEntry);
  rpc Tail(TailRequest) returns (stream LogEntry);
  rpc VerifyRange(VerifyRangeRequest) returns (VerifyRangeResponse);
  rpc ExportAuditReport(ExportAuditReportRequest) returns (AuditReport);
  rpc GetStats(GetStatsRequest) returns (GetStatsResponse);
}
```

`protoc-gen-go-grpc` turns that into a Go **interface** the server implements:

```go
type LogServiceServer interface {
	Query(*QueryRequest, grpc.ServerStreamingServer[LogEntry]) error
	Tail(*TailRequest, grpc.ServerStreamingServer[LogEntry]) error
	VerifyRange(context.Context, *VerifyRangeRequest) (*VerifyRangeResponse, error)
	ExportAuditReport(context.Context, *ExportAuditReportRequest) (*AuditReport, error)
	GetStats(context.Context, *GetStatsRequest) (*GetStatsResponse, error)
	mustEmbedUnimplementedLogServiceServer()
}
```

> **Go concept — a service is an interface.** The RPC contract becomes a Go
> `interface`; doc 12's `grpcapi` package implements it. Note the shapes: the
> two `stream LogEntry` RPCs (`Query`, `Tail`) take a
> `grpc.ServerStreamingServer[LogEntry]` you push results into and return only an
> `error`; the three unary RPCs take a `context.Context` plus a request pointer
> and return a response pointer and an `error` — the `(*T, error)` pair you saw
> in doc 02. The `mustEmbedUnimplementedLogServiceServer()` line forces
> implementers to embed a generated base struct, so adding a new RPC to the
> `.proto` later won't break existing servers at compile time — forward
> compatibility for the *server code*, mirroring the wire's forward
> compatibility.

---

## 3. Deep dives

### 3.1 Schema-first: one contract, many languages

The order of operations is the whole point. You do **not** write a Go
`LogEntry`, a C++ `LogEntry`, and a Python `LogEntry` and hope they agree. You
write *one* `LogEntry` in `log.proto` and generate all three. Each language's
generator applies its own idiomatic mapping (Go gets structs and slices, C++
gets classes with accessors, Python gets classes with descriptors), but they
all encode to the identical bytes on the wire, because the *field numbers and
types* — not the field names, not the language — define the encoding.

For devlogd this means a C++ sanitization station and the Go service literally
cannot disagree about what a `LogEntry` contains: there is one definition, and
CI's `buf breaking` guards it. When you need to evolve the record, you change
`log.proto`, run `buf generate`, and every language picks up the new field on
its next build. This is why the generated code is a *component* worth its own
doc even though nobody writes it by hand — it is the contract the entire system
is bolted to.

### 3.2 Backward & forward compatibility: field numbers and unknown fields

Look again at `log.proto`'s `LogEntry`:

```proto
  SanitizationEvent sanitization = 12;
  reserved 13, 14;                 // held for future first-class fields
  Audit audit = 15;                // filled by the server
```

Two mechanisms make old and new binaries interoperate:

**Field numbers are the contract.** On the wire, a field is identified by its
*number*, never its name. So you may freely rename `message` to `text` in the
schema (a source change for humans) without touching a single byte on the wire.
The corollary is the ironclad rule stated in the schema's own comment and
enforced by `buf breaking`: **only add fields with fresh numbers; never
renumber or retype an existing field; and `reserve` the numbers of removed
fields** so no future edit can accidentally reuse a retired number with a new
meaning. That is why `13, 14` are `reserved` — they are pre-burned slots. Adding
`audit = 15` was purely additive: an older client that predates it simply
doesn't know field 15 exists.

**Unknown-field preservation.** That older client does something better than
ignore field 15 — it *keeps* it. Remember `unknownFields protoimpl.UnknownFields`
in the generated struct? When a binary decodes a message containing a field
number it doesn't recognise, it stashes the raw bytes there and re-emits them
verbatim on encode. So a message can round-trip through an old intermediary
without losing the fields that intermediary never understood. In devlogd this is
load-bearing for integrity: an entry's hash is computed over its full canonical
bytes, so preserved unknown fields **stay inside the signed hash** and
verification still succeeds across version skew. This is what "backward *and*
forward compatible" actually means in practice.

**Server-owned fields.** Compatibility rules govern the schema; a separate
convention governs *who fills what*. `entry_id`, `ingest_time`, and `audit` are
**server-owned**: producers must leave them empty, and the ingest pipeline
(doc 09) overwrites them regardless of what a producer sent. So a lying or buggy
client cannot forge an id, a timestamp, or a signature — the field exists in the
shared schema, but authority over its value lives on the server.

---

## 4. Idioms & gotchas

- **Never hand-edit `*.pb.go`.** The `DO NOT EDIT` header is literal;
  re-running `buf generate` will silently discard your changes. Change the
  `.proto` and regenerate.
- **`nil` vs zero for message fields.** `entry.Audit == nil` means "unset";
  `entry.Audit != nil && len(entry.Audit.Signature) == 0` means "present but
  empty". Confusing the two is the classic proto bug. Reach for `GetAudit()`
  when a link might be nil.
- **You can't distinguish unset from zero for scalars.** In proto3 a `Seq` of 0
  and an absent `Seq` are the same. If you truly need "was it set?", model it as
  a sub-message (pointer) — that's the design reason timestamps are pointers.
- **Import path ≠ package name.** The directory is imported as
  `devlog/api/gen/devicelog/v1`, but the identifier is `devicelogv1` (from the
  `;devicelogv1` suffix in `go_package`). Let `goimports` write the import for
  you and don't guess.
- **Field numbers `1–15` are cheap; `16+` cost an extra byte.** Reserve the low
  numbers for hot fields. `audit = 15` uses the last one-byte tag deliberately.
- **Generated code is committed on purpose.** If you `.gitignore`d `api/gen`,
  `go build` would fail for anyone without the proto toolchain. Commit it and
  regenerate as a reviewed change.

---

## 5. Exercises (zero → hero)

1. **Recall.** Why is `DeviceTime` a `*timestamppb.Timestamp` (pointer) while
   `Seq` is a plain `uint64`? What can the pointer express that the value
   cannot?
2. **Recall.** On the wire, what identifies the `message` field — its name or
   its number? What follows for renaming a field versus renumbering it?
3. **Apply.** Add a new optional field `string firmware_version = 16;` to
   `LogEntry`. Which file do you edit, what command regenerates the Go, and why
   is this change safe for an already-deployed older client to receive?
4. **Extend.** A teammate proposes reusing field number `13` (currently
   `reserved`) for a new field. Explain, referencing `buf breaking` and the
   unknown-field mechanism, why the `reserved` marker exists and what would
   break if you removed it and shipped conflicting definitions to two clients.
5. **Hero.** Trace `entry.GetSanitization().GetVerification().GetPassed()` for a
   `LogEntry` whose `Sanitization` is `nil`. Explain, using the generated
   getters in §2.6, why this returns `false` instead of panicking — and rewrite
   it with direct field access, showing exactly where the nil-pointer panic
   would occur.

---

## 6. Recap & next

You now know how Go code is organised — a **module** (`go.mod`) containing
**packages** reached by **import paths** — and how the types the rest of devlogd
is built on are *generated*, not written: proto messages become structs
(pointers for sub-messages, values for scalars), enums become defined `int32`
types with typed constants, `repeated`/`map`/`bytes` map to slices/maps/byte
slices, and every field carries a wire number that is the real, name-independent
contract. You've seen why generated code is committed, how nil-safe getters keep
optional chains panic-free, and how field numbers plus unknown-field
preservation make old and new binaries interoperate.

**Next:** [02 — config](02-config.md), your first hand-written Go package, where
these ideas — defined types, structs, struct tags, pointers, and Go's error
model — reappear in code you author yourself.
