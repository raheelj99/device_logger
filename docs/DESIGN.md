# Design — Architecture, Patterns, and Why This Is Production-Grade

## Guiding principles

Five rules the patterns below exist to serve. The thesis is narrow and
testable: **every decision is deliberate, every claim is verifiable, and every
failure mode is named before it happens.** Cleverness is not the goal —
control is.

| Principle | What it means in practice | Where you can see it |
| --- | --- | --- |
| **Contracts before code** | One schema (`api/proto`) generates the Go service *and* every client — C++, Node, Nest, Python. Drift is impossible, not merely discouraged. | `clients/proto` is the same `.proto` the server compiles from; client unit tests assert a byte round-trip through it. |
| **Fail fast, fail loud** | Bad config, unreachable dependencies, or a broken chain abort at the earliest possible point with a precise message — never a silent wrong answer. | `config.validate()`, devlogd's startup pings, the C++ supervisor detecting an early child exit (§7), checksum/chain verification. |
| **Trust is earned per entry** | Security is not a perimeter; it is a property carried by each record: signed, hash-chained, license-gated, TLS-only. | `internal/sign`, per-session license auth, mutual-TLS option. |
| **Every claim is verifiable** | Nothing is asserted that a test or a runtime probe cannot confirm — including "it works from another language." | Go `-race` tests, client codec round-trip tests, the cross-language e2e harness, `/readyz`. See [`TESTING.md`](TESTING.md). |
| **Name the failure first** | Each component's failure mode and blast radius is documented and bounded *before* it is relied upon. | §5 below; the C++ supervisor's fail-fast policy (§7); drop-on-slow tail. |

Everything below — the pattern inventory, the security model, and the
deployment and client integrations — is an application of one of these five.

## 1. System overview

```mermaid
flowchart LR
    CPP[C++ sanitization app] -- "MQTT 8883 / TLS<br/>license as credential" --> B
    subgraph devlogd [devlogd — one static binary]
        B[embedded MQTT broker<br/>auth hook + topic ACL] --> P[ingest pipeline<br/>validate → ULID → sign+chain]
        P --> HUB[live hub]
        P --> HOT[(Redis Streams<br/>hot tier / WAL)]
        ARC[archiver<br/>consumer group] --> COLD[(MinIO bucket<br/>zstd segments + manifests)]
        HOT --> ARC
        Q[query engine<br/>merge + dedupe + verify]
        HOT --> Q
        COLD --> Q
        G[gRPC LogService<br/>TLS + license interceptors]
        Q --> G
        HUB --> G
    end
    OP[logctl / any gRPC client] -- "gRPC 9443 / TLS" --> G
    LS[licensed<br/>mini license server] -. "activate/heartbeat<br/>(online mode)" .-> devlogd
    PROM[Prometheus] -- scrape :9090 --> devlogd
    PROM --> GRAF[Grafana dashboard]
```

**Write path.** MQTT CONNECT is authenticated against the license (signature,
window, subject, feature). The topic ACL pins a session to
`devlog/v1/<its-own-id>/#`. Each publish is decoded, identity-checked, given a
ULID and ingest timestamp, hashed (SHA-256 over the canonical entry), linked
to the device's previous hash, Ed25519-signed, and appended to the device's
Redis Stream in the same transaction that advances the chain head. Ack to the
producer happens after Redis has it (AOF-persisted).

**Archival.** A consumer group drains the streams in the background, batching
by size/time into zstd-compressed segments of length-prefixed protobuf, each
with a JSON manifest (time range, devices, seq ranges, SHA-256). Ack happens
only after the segment is durable — crash-safe, at-least-once.

**Read path.** Queries prune manifests by time/device, fetch only matching
segments (checksum-verified), merge with the hot tier, deduplicate by entry
ID, sort, cap, stream. Tail is an in-process fan-out with drop-on-slow
semantics. Verification re-computes every hash, signature, and chain link.

## 2. Pattern inventory — what's used and why

| Pattern | Where | Why it earns its place |
| --- | --- | --- |
| **Ports & adapters (hexagonal)** | `cold.ObjectStore` interface; `MinioStore` adapter vs `fakeStore` in tests | storage tech is swappable (S3, GCS, Azure) without touching the pipeline; tests run without infrastructure |
| **Composition root** | `cmd/devlogd/main.go` builds every component and injects dependencies | zero global state; each package is independently constructible and testable |
| **WAL-style buffering** | Redis Streams between ingest and archiver | producers get fast acks; cold-storage latency/outages never back-pressure a robot in the field |
| **Consumer group + ack-after-durable** | `internal/archive` | at-least-once delivery to the bucket across crashes; the lost-update window is zero |
| **Idempotent reads (dedupe by ULID)** | `internal/query` | converts at-least-once storage into exactly-once observation — simpler than distributed transactions and equally correct here |
| **Manifest-indexed immutable segments** | `internal/cold` | O(days) listing instead of O(objects); segments are write-once, checksummed, cache-friendly; manifest-written-last means no partial segment is ever visible |
| **Hash chain + per-entry signature** | `internal/sign` | tamper-*evidence*, not just tamper-resistance: any edit, deletion, or reorder breaks the chain at a provable point |
| **Interceptor chain (middleware)** | `internal/grpcapi/server.go` | auth, panic-recovery, and metrics are one implementation applied uniformly — impossible to forget on a new RPC |
| **Structured concurrency** | `errgroup` + `signal.NotifyContext` in main | one Ctrl-C or one fatal component error unwinds every goroutine gracefully, with a final archiver flush |
| **Fail-fast config** | `internal/config` validation | misconfiguration dies at startup with a precise message, never at 3 a.m. mid-request |
| **12-factor config** | YAML + `DEVLOG_*` env overrides | same binary in dev, compose, and fleet deployment |
| **Observability-first** | `internal/telemetry`; every component takes `*Metrics` and `*slog.Logger` | latency histograms, error reasons, and domain counters (sanitization phases!) are first-class, not bolted on |
| **Schema-first, generated contract** | `api/proto` + buf | Go service and C++ producer compile from the same file; backward compatibility is a protobuf invariant, linted by `buf breaking` |
| **Best-effort fan-out with drop policy** | `internal/ingest/hub.go` | an explicit decision: live tails are a convenience view; a slow observer must never stall ingestion — history remains authoritative |

## 3. Security model (zero-trust posture)

- **Transport**: TLS ≥ 1.2 on MQTT, gRPC, and the license server; optional
  mutual TLS on both listeners (`client_ca_file`). Dev PKI via `tools/gencerts`;
  swap in your real PKI by pointing config at other PEM files.
- **Authentication = licensing**: sessions exist only with a valid
  Ed25519-signed license — offline-verifiable (air-gap friendly) with optional
  online activation, session caps, and a bounded grace window. The license is
  scoped: subject binding (device identity) and feature grants (`ingest` vs
  `query`) separate the write plane from the read plane.
- **Authorization**: MQTT topic ACL confines every session to its own device
  namespace; the pipeline independently rejects payloads whose `device_id`
  contradicts the authenticated identity (defense in depth).
- **Integrity**: SHA-256 + Ed25519 per entry; per-device hash chain; SHA-256
  per segment; Ed25519 over each exported audit report. Key IDs everywhere
  allow rotation without invalidating history.
- **Least surface**: one static binary, distroless container (no shell),
  `internal/` packages unimportable from outside, secrets gitignored, signing
  key readable only by the service.

Trust boundaries and residual assumptions: devlogd's host is the trust anchor
(it holds the signing key — an attacker with root there can sign what they
like *going forward*, but cannot rewrite history without breaking the chain);
Redis and MinIO are treated as untrusted-for-integrity (tampering is detected,
not prevented — pair with standard disk/network encryption for confidentiality).

## 4. Compliance mapping (NIST SP 800-88 Rev. 1 / IEEE 2883-2022)

The standards require that sanitization be *documented and verifiable*. The
mapping from their documentation expectations to mechanisms here:

| Requirement (summarized) | Mechanism |
| --- | --- |
| Record media identity (serial, model, capacity, type) | `TargetMedia` in every `SanitizationEvent` |
| Record sanitization method & technique (Clear/Purge/Destroy) | `SanitizationStandard` + `technique` enums/fields — NIST and IEEE vocabularies both first-class |
| Record verification performed and its result | `Verification{method, sample_pct, passed}` on the COMPLETED event |
| Record personnel and time | `operator_id`, `device_time`, server-side `ingest_time` |
| Records must be trustworthy / auditable | per-entry Ed25519 signature + hash chain; `VerifyRange` proves integrity of any window |
| Produce a certificate per sanitized device | `ExportAuditReport(trace_id)` → signed JSON bundle of the full job, ready to render into a Certificate of Sanitization |
| Retention of records | bucket tier is the durable system of record; retention = bucket lifecycle policy |

**Robotics-readiness**: nothing above is hard-wired to sanitization. The core
entry is generic (`severity`, `subsystem`, `attributes`, `payload`, `trace_id`
as mission id); `SanitizationEvent` is one optional typed extension — a
`NavigationEvent` or `ArmTelemetry` message can be added tomorrow as field 13
or 14 without breaking a single stored entry or deployed producer.

## 5. Failure modes

| Failure | Behavior | Data at risk |
| --- | --- | --- |
| devlogd crash | producers reconnect (MQTT QoS 1 retries in-flight publishes); archiver reclaims unacked stream entries on restart | none acked |
| Redis down | ingest fails → producer retries; queries serve cold tier | none (producer-side buffering is the C++ app's QoS queue) |
| MinIO down | ingest unaffected; archiver retains batch and retries; hot tier keeps absorbing (`hot_retention` of headroom) | none until hot retention is exceeded during a very long outage |
| License server down (online mode) | previously activated sessions continue up to `grace`; new sessions denied | availability trade-off, chosen deliberately |
| Segment corrupted / bucket tampered | checksum mismatch fails the read loudly; chain verification pinpoints missing/altered entries | detection guaranteed, recovery from bucket versioning/backup |
| Slow tail consumer | entries dropped from that live stream only | none — history intact |
| Clock skew on producers | ordering/retention use server `ingest_time`; `device_time` preserved as evidence | none |

## 6. Deliberate simplifications (documented, not accidental)

- **Single devlogd instance.** The hash chain and live hub assume one writer.
  Scaling path: shard devices across instances (chains are per-device, so
  sharding is trivial); the storage layout already supports it.
- **Query collects-then-sorts** with a server-side cap (`query.max_results`)
  instead of a streaming merge-sort across tiers. Right trade-off at
  fleet-log scale; the gRPC contract (server streaming) already permits a
  smarter engine later without breaking clients.
- **Canonical hash = deterministic protobuf marshal** of the entry (audit
  block excluded). Stable given the pinned protobuf runtime; the runtime is
  version-locked in `go.mod`/`go.sum` and re-verification runs through the
  same binary. A cross-language canonical encoding was rejected as complexity
  without a driving requirement — verification is a service-side operation.
- **In-memory session table** in `licensed`. Restart forgets sessions —
  acceptable because activation is idempotent and re-activation is automatic
  via heartbeat; a persistent store would add state for negligible gain at
  this scale.

## 7. Deployment integration — the C++ application owns devlogd's lifecycle

**What.** `deploy/embed/` ships a dependency-free `DevlogdSupervisor` (POSIX +
libstdc++ only) that the sanitization app uses to spawn devlogd during its own
init, block until devlogd is actually ready, and drain it gracefully on
shutdown. Full mechanics and `.deb` packaging:
[`deploy/embed/README.md`](../deploy/embed/README.md).

**Why this shape.**

- *Lifecycle coupling was the requirement*: the audit logger must live and die
  with the sanitization app, on hosts that may not run systemd (field robots,
  minimal containers) — so the app owns the process, not systemd.
- *Readiness, not liveness*: `start()` gates on `GET /readyz == 200`, not "the
  process exists." A logger that's up but can't reach Redis is useless — fail
  fast applies to a dependency you can't sanitize media without.
- *Graceful drain*: `stop()` sends `SIGTERM` (devlogd flushes the archiver),
  waits a bounded grace window, then escalates to `SIGKILL` — bounded, never
  hanging.
- *Zero third-party deps*: the readiness probe is a raw-socket HTTP GET rather
  than linking libcurl — fewer dependencies on the appliance, smaller attack
  surface, simpler packaging.

**How it's proven.** Compiles under `-std=c++17`; runtime-tested against a
fake devlogd (spawn → readiness-gate → graceful `SIGTERM` stop). The `.deb`
guidance encodes the security boundary as file permissions (signing key
`0600`, dedicated `devlog` user, `issuer.key` never shipped).

## 8. Multi-language clients — one contract, five reference implementations

**What.** `clients/` ships reference clients in Node, NestJS, and Python
(Django/Flask/FastAPI over a shared `devlog_client` package). Each exercises
*both* planes: publish a full sanitization job over MQTT/TLS, then read it
back over gRPC/TLS (query, verify the chain, export the signed audit report).
Per-stack usage: [`clients/README.md`](../clients/README.md).

**Why both planes.** A client that only queries proves nothing about
ingestion, and vice versa. The `e2e` command in every client is the real
contract: write from a non-Go client, read it back, and confirm the
signatures the Go service produced still verify — the only test that proves
the module is genuinely polyglot-ready.

**Why a shared, verified reference.** The Node client was built and tested
first, then used as the exact template for the others — one reference,
mirrored, so five clients can't quietly diverge in how they authenticate, name
topics, or encode entries.

**How correctness is anchored.** Every client's unit tests do a protobuf
round-trip through the shared `.proto` and assert field numbers/types — the
same bytes the Go server parses. No server, Redis, or MinIO needed, so these
run anywhere and fast (see [`TESTING.md`](TESTING.md) §3).

**The boundary every client refuses to cross.** None can set `entry_id`,
`ingest_time`, or `audit` — those are server-owned, and each client's builder
is structurally incapable of populating them (tested). This is
*trust-is-earned-per-entry* holding no matter which language calls in.

**Limit, stated plainly.** These are reference integrations, not published
SDKs — complete and correct against the contract, but versioning/packaging
for distribution is left to the consuming team.

## 9. Self-hosted backing services (opt-in) — `internal/bootstrap`

**What.** `redis.auto_start`/`s3.auto_start` (both disabled by default) let
devlogd spawn its own `redis-server`/`minio` at startup when the configured
address isn't reachable yet, instead of only ever failing fast. See
[`MANUAL.md`](MANUAL.md) §2a for operator-facing setup.

**Why opt-in, not automatic.** Changing devlogd's default startup behavior
would be a silent regression for anyone already pointing it at an externally
managed, possibly shared Redis/MinIO — *fail fast* stays the default. An
operator turns this on explicitly for a single-station appliance that isn't
handed an already-running instance.

**Why spawn the real binaries, not an embedded fallback.** The alternative —
an in-process Redis stand-in for the hot tier — would be in-memory only and
would not survive a crash, quietly weakening the durability guarantee this
whole design is built around (§2's WAL-style buffering, the hash chain's
tamper-*evidence*). Spawning the real `redis-server` (with `--appendonly yes`)
and `minio` against persistent data directories keeps every guarantee above
unchanged; it also matches the project's own deployment story — the `.deb`
packaging in [`deploy/embed/README.md`](../deploy/embed/README.md) already
lists `redis-server` and `minio` as dependencies installed alongside devlogd
on the appliance.

**Scope, stated plainly.** This is a startup-time convenience, not a runtime
supervisor: if Redis or MinIO die once devlogd is already running, devlogd
does not try to respawn them — §5's failure modes still apply unchanged.
devlogd only stops a process it itself spawned; an externally managed
instance it merely connected to is never signaled.
