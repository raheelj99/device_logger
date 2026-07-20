# Component Reference

A technical walk through every component of devlogd: what it is, the concepts it
rests on, its public surface, how it connects to its neighbours, and how it
behaves when things go wrong.

This complements the other docs rather than repeating them:
[`DESIGN.md`](DESIGN.md) argues *why* the architecture is shaped this way,
[`GO_CONCEPTS.md`](GO_CONCEPTS.md) teaches the Go language features for a C++
engineer, [`MANUAL.md`](MANUAL.md) is the operator runbook, and
[`TESTING.md`](TESTING.md) documents how all of this is verified.

> **Going deeper — one document per component.** For a *zero-to-hero* Go
> walkthrough of each component below (every keyword, type, idiom, and
> stdlib call explained from first principles, with C++ contrasts and
> exercises), see the [`components/`](components/README.md) series — an ordered
> learning path from the proto contract to the composition root.

---

## 0. The big picture in one paragraph

devlogd is **one static Go binary** with two network planes. The **write plane**
accepts structured log entries over MQTT/TLS, authenticates each session against
a signed license, then signs and hash-chains every entry and appends it to a
per-device Redis Stream. A background **archiver** drains those streams into
compressed, checksummed, manifest-indexed segments in an S3 bucket. The **read
plane** serves historical queries (hot Redis + cold bucket, merged and
deduplicated), live tails, tamper verification, and signed audit reports over
gRPC/TLS. Everything is observable via Prometheus.

```
                         ┌──────────────────────── devlogd (one process) ───────────────────────┐
 C++ station ─MQTT/TLS─▶ │ broker → ingest.Pipeline ─┬─▶ hot.Store (Redis Streams)               │
 (license = password)    │ (auth+ACL hooks)          │        │                                   │
                         │                           └─▶ ingest.Hub (live)   archive.Archiver     │
                         │                                       │              │ (consumer group)│
                         │                                       │              ▼                  │
 operator ──gRPC/TLS───▶ │ grpcapi.Service ◀── query.Engine ◀────┴───── cold.Store (MinIO bucket) │
 (license = bearer)      │  (interceptors)                                                         │
 Prometheus ─HTTP──────▶ │ telemetry.HTTP (/metrics /healthz /readyz)                             │
                         └───────────────────────────────────────────────────────────────────────┘
                     licensed (separate binary, online mode)   lictl / logctl / gencerts (CLIs)
```

**Cross-cutting concepts used everywhere**

- **Ports & adapters (hexagonal).** Storage is reached through an interface
  (`cold.ObjectStore`); the real MinIO adapter and the in-memory test fake are
  interchangeable. Business logic never imports a vendor SDK directly.
- **Composition root.** `cmd/devlogd/main.go` is the only place that constructs
  concrete types and wires them together; every package takes its dependencies
  as parameters (dependency injection), so there is zero global state.
- **`context.Context` everywhere.** Cancellation and deadlines propagate as an
  explicit first argument, so one signal unwinds every goroutine and every
  blocking I/O call.
- **Fail-fast.** Misconfiguration and unreachable dependencies abort at startup
  with a precise error, never mid-request.

---

## 1. The contract — `api/proto/devicelog/v1`

**Files:** `log.proto` (data), `query.proto` (service), `api/gen/**` (generated
Go, committed).

**Responsibility.** The single schema both the Go service and every client
(C++, Node, Nest, Python) are generated from. There is only one definition of a
`LogEntry` in the whole system, so wire drift between producer and consumer is
structurally impossible.

**Key messages.**

- `LogEntry` — the universal record: identity (`device_id`, `seq`), timing
  (`device_time` from the producer, `ingest_time` from the server), `severity`,
  `subsystem`, `message`, an open `map<string,string> attributes`, an optional
  `bytes payload`, a `trace_id` (the sanitization job / robot mission id), an
  optional typed `SanitizationEvent`, and a server-filled `Audit` block.
- `Audit` — the tamper-evidence: `entry_hash` (SHA-256), `prev_hash` (link to
  the device's previous entry), `signature` (Ed25519), `key_id`.
- `SanitizationEvent` — NIST SP 800-88 / IEEE 2883 domain model: target media,
  standard, technique, phase, progress, verification, operator.
- `LogService` — five RPCs: `Query` and `Tail` (server-streaming),
  `VerifyRange`, `ExportAuditReport`, `GetStats` (unary).

**Concepts.**

- **proto3 field numbers are the contract, not the names.** Wire compatibility
  depends only on the tag number and type. Rule enforced here: only *add*
  fields with fresh numbers, never renumber or retype, and `reserve` the numbers
  of removed fields. Fields `13, 14` are reserved for future first-class
  extensions.
- **Unknown-field preservation.** An old binary that receives a newer entry
  carries fields it doesn't understand through untouched — they even remain
  inside the signed hash. This is what makes the schema *backward and forward
  compatible*.
- **Server-owned fields.** `entry_id`, `ingest_time`, and `audit` are filled by
  the server; producers must leave them empty. The ingest pipeline overwrites
  them regardless, so a lying producer cannot forge them.
- **Code generation** is driven by `buf` (`buf.gen.yaml`); `buf lint` and
  `buf breaking` can guard the compatibility rules in CI.

---

## 2. Composition root — `cmd/devlogd`

**File:** `main.go`.

**Responsibility.** Load config, construct every component in dependency order,
inject them, and run all long-lived goroutines under one supervisor.

**Concepts.**

- **`errgroup.WithContext`** — structured concurrency. The broker, archiver,
  Redis janitor, HTTP server, and gRPC server each run in `g.Go(...)`. The
  *first* one to return an error cancels the shared `ctx`, which unwinds all the
  others; `g.Wait()` returns that first error. There is no orphaned goroutine.
- **`signal.NotifyContext`** — turns `SIGINT`/`SIGTERM` into `ctx` cancellation,
  so Ctrl-C and `systemctl stop` follow the exact same graceful-shutdown path.
- **Graceful gRPC stop** — a dedicated goroutine waits on `<-ctx.Done()` then
  calls `grpcServer.GracefulStop()`, draining in-flight RPCs.
- **Readiness flag** — an `atomic.Bool` flips to true only after every component
  is constructed; `/readyz` reports it (this is exactly what the C++ supervisor
  and Kubernetes-style probes gate on).

---

## 3. Configuration — `internal/config`

**Files:** `config.go`, `tls.go`.

**Responsibility.** Load YAML, apply environment overrides, validate, and build
`*tls.Config` objects.

**Public surface.** `Load(path) (*Config, error)`, `ServerTLS(TLS)`,
`ClientTLS(caFile)`, and the `Duration` type.

**Concepts.**

- **12-factor config.** Values come from a YAML file, then `DEVLOG_*`
  environment variables win over the file (`applyEnv`), so the same binary runs
  unchanged in dev, compose, and the field.
- **Custom `yaml.Unmarshaler`.** `Duration` wraps `time.Duration` so YAML can
  say `24h` / `500ms`; a parse error there fails the load with a clear message.
- **Fail-fast validation.** `validate()` requires the security-critical paths
  (cert/key files, signing key, issuer key, S3 endpoint) to be present, and
  rejects an `online` mode with no `server_url` and any unknown mode. The
  service refuses to start misconfigured rather than failing later at runtime.
- **TLS construction.** `ServerTLS` pins `MinVersion: TLS1.2`; supplying a
  `client_ca_file` upgrades the listener to **mutual TLS**
  (`RequireAndVerifyClientCert`). `ClientTLS` builds a CA-pinned client config.

---

## 4. Observability — `internal/telemetry`

**Files:** `logging.go`, `metrics.go`, `http.go`.

**Responsibility.** The service's own logs, metrics, and health endpoints.

**Concepts.**

- **Structured logging with `slog`.** JSON handler → machine-parseable logs for
  Loki/ELK. Log level is parsed from config.
- **Metrics as an injected struct, not globals.** `NewMetrics()` builds a fresh
  `prometheus.Registry` and registers every counter/gauge/histogram via
  `promauto.With(reg)`. Passing `*Metrics` around (instead of package globals)
  keeps ownership explicit and makes tests isolated — each test gets its own
  registry with no double-registration panic. Domain metrics are first-class:
  sanitization phase counts, verification pass/fail, per-device ingest.
- **Health semantics.** `/healthz` = liveness (process is up); `/readyz` =
  readiness (fully started, backed by the `atomic.Bool`); `/metrics` = the
  Prometheus scrape endpoint. `Run(ctx)` serves until cancelled then shuts down
  with a timeout.

---

## 5. Crypto core — `internal/sign`

**Files:** `sign.go`, `keys.go`.

**Responsibility.** The tamper-evidence primitives: per-entry hashing, signing,
and the per-device hash chain.

**Public surface.** `Signer` (`Sign`, `Public`, `KeyID`), `Verifier`
(`Add`, `Verify`), `EntryHash`, `ChainSign`, `VerifyEntry`, and PEM helpers.

**Concepts.**

- **Canonical hashing.** `EntryHash` clones the entry, nils the `Audit` block
  (it holds the hash itself), and marshals with
  `proto.MarshalOptions{Deterministic: true}`, then SHA-256s the bytes. Same
  content ⇒ same digest, so re-verification is reproducible.
- **Ed25519 signatures.** Fast, small (64-byte) signatures over the entry hash.
- **Hash chain.** `ChainSign` sets `entry_hash`, links `prev_hash` to the
  device's previous entry hash, and signs. Any edit, deletion, or reorder breaks
  the chain at a *provable* point — this is tamper *evidence*, not just
  resistance.
- **Key rotation via key IDs.** `Verifier` is a `map[keyID]publicKey`. Each
  entry records the `key_id` that signed it, so rotated keys keep verifying the
  history they signed — you add the retired public key to the verifier set.
- **PEM/PKCS8/PKIX** helpers marshal and load keys; type mismatches are rejected
  with a precise error.

---

## 6. Authentication — `internal/license`

**Files:** `license.go`, `manager.go`, `online.go`, `server.go`.

**Responsibility.** The license *is* the credential. A session exists only with
a valid Ed25519-signed license, presented as the MQTT password and the gRPC
bearer token.

**Concepts.**

- **Signed, self-describing token.** A `License` (subject, features, validity
  window, max sessions) plus an Ed25519 signature over its canonical JSON, then
  base64-encoded to a single line — that line is the `.lic` file.
- **Offline verification** (`Verifier.Verify`) checks signature, validity window
  (with ±5 min clock skew), **subject binding** (the license subject must equal
  the device id, or be `*`), and **feature grant** (`ingest` vs `query`). This
  works air-gapped — right for field robots and secure labs.
- **`Manager`** is the single authorization entry point for both planes. It
  always runs offline verification, and — in online mode — additionally calls
  the validator. It also tracks the active-session gauge.
- **Online validation with a grace window** (`OnlineValidator`). Posts
  `/v1/activate` to the license server. If the server is *unreachable*, a
  license that activated successfully before is allowed for `grace` (default
  72h) — availability chosen deliberately so a robot doesn't lose logging when
  the license server blips. A *definitive* rejection (HTTP 403) always denies.
- **`Server`** (used by `cmd/licensed`) re-verifies tokens and enforces
  `max_sessions` per license across devices, with an idempotent
  activate/heartbeat endpoint.

**Separation of planes.** `ingest` and `query` are distinct features, so a
station license (write) cannot read and an operator license (read) cannot write.

---

## 7. Ingestion transport — `internal/broker`

**Files:** `broker.go`, `hooks.go`.

**Responsibility.** Embed the MQTT broker (`mochi-mqtt`) inside the process:
TLS termination, license auth, topic ACLs, and handoff of payloads to the
pipeline.

**Concepts.**

- **Embedded broker, no external MQTT dependency.** The broker is a library
  running on a goroutine inside devlogd.
- **Hook chain (middleware for MQTT).** Two hooks implement the `mqtt.Hook`
  interface via `Provides(bit)`:
  - `authHook` — `OnConnectAuthenticate` maps `username → device id`,
    `password → license token`, and calls `Manager.Authorize(..., FeatureIngest)`.
    `OnACLCheck` permits **publish only**, and only under
    `devlog/v1/<own-device-id>/…`, so one compromised device can't write as
    another. `OnSessionEstablished/OnDisconnect` maintain the connection gauge.
  - `ingestHook` — `OnPublish` parses the subsystem from the topic
    (`devlog/v1/{device}/{subsystem}`) and calls `pipeline.Ingest`. A rejected
    publish is logged but does **not** drop the session (a bad message shouldn't
    disconnect a healthy producer).
- **Defense in depth.** The topic ACL confines a session; the pipeline
  *independently* rejects an entry whose `device_id` disagrees with the
  authenticated identity.

---

## 8. Ingest pipeline & live hub — `internal/ingest`

**Files:** `pipeline.go`, `hub.go`.

**Responsibility.** Turn an authenticated raw payload into a signed, chained,
stored entry, and fan freshly ingested entries out to live tail subscribers.

**Concepts.**

- **The critical section.** Each entry must link to the device's *true* previous
  hash, so `chain-read → sign → append` runs under one mutex per pipeline. An
  in-memory `chains map[device][]byte` caches the head to avoid a Redis read per
  entry; on a store failure the cache entry is dropped so the next entry re-reads
  the durable head.
- **Server-owned fields enforced.** The pipeline assigns a **ULID** `entry_id`
  (lexicographically sortable, time-ordered, collision-resistant), stamps
  `ingest_time`, defaults `device_id`/`subsystem` from the session, and clears
  any producer-supplied `audit`.
- **Ack-after-durable at the producer edge.** `AppendWithChain` persists to
  Redis (AOF) *before* the MQTT ack returns, so an acknowledged entry survives a
  crash.
- **Best-effort fan-out with a drop policy.** `Hub.Publish` does a
  non-blocking send to each subscriber channel; a slow tail consumer drops
  entries rather than stalling ingestion — history in storage stays
  authoritative. This is a deliberate, documented trade-off.
- **Observability.** The pipeline records latency, byte counts, per-severity
  and per-sanitization-phase counters as it goes.

---

## 9. Hot tier — `internal/hot`

**File:** `store.go`.

**Responsibility.** The short-term tier: one **Redis Stream per device**, which
doubles as the durable buffer (WAL) the archiver drains.

**Concepts.**

- **Redis Streams as an append log.** `AppendWithChain` uses a **transactional
  pipeline** (`MULTI/EXEC`) to `XADD` the entry, advance the chain head
  (`HSET`), and register the device (`SADD`) atomically. The stream **ID
  encodes the ingest time in milliseconds**, so time-range reads (`XRANGE`) and
  retention trimming (`XTRIMMINID`) operate directly on IDs.
- **WAL pattern.** Producers get fast acks from Redis; cold-storage latency or
  outages never back-pressure a robot in the field.
- **Consumer group operations** for the archiver: `EnsureGroup`,
  `ReadGroup` (blocking `XREADGROUP` across devices), `Ack` (`XACK`), and
  `ClaimStale` (`XAUTOCLAIM`) to adopt entries a crashed consumer read but never
  acked → at-least-once delivery to cold storage.
- **Retention janitor.** `RunJanitor` periodically `Trim`s entries older than
  `hot_retention`; cold storage is the system of record beyond that window.
- **Stats.** `XLEN` + `XREVRANGE` per device for the `GetStats` RPC.

---

## 10. Cold tier — `internal/cold`

**Files:** `segment.go`, `store.go`.

**Responsibility.** The long-term tier: compressed, immutable, checksummed,
manifest-indexed segments in an S3-compatible bucket.

**Concepts.**

- **Segment format.** `EncodeSegment` writes `zstd( varint-length ‖ protobuf,
  … )` — each entry length-prefixed with a **uvarint**, the whole stream
  zstd-compressed. `DecodeSegment` streams entries back, guarding against
  corrupt/hostile length prefixes (`maxEntryBytes`).
- **Manifest indexing.** Each segment has a small JSON `Manifest` (time range,
  per-device seq ranges, count, compressed size, SHA-256). Queries prune by
  manifest — `Overlaps(from,to)` and `HasAnyDevice(devices)` — so a time/device
  query lists a handful of manifests instead of scanning every object. Listing
  is organized by day (`segments/YYYY/MM/DD/…`).
- **Manifest-written-last.** `Writer.Write` puts the segment first, then the
  manifest. A crash between the two leaves an orphan segment that no query can
  see — **no partial segment is ever visible**.
- **Integrity on read.** `Reader.Read` fetches a segment, recomputes SHA-256,
  and fails loudly on a mismatch (bucket tampering or corruption). Segments are
  write-once and immutable.
- **Ports & adapters.** `ObjectStore` is the interface (`Put`/`Get`/`List`);
  `MinioStore` is the production adapter (auto-creates the bucket). Swapping in
  GCS/Azure/S3 is a new adapter, untouched pipeline. Tests use an in-memory
  fake.

---

## 11. Archiver — `internal/archive`

**File:** `archiver.go`.

**Responsibility.** Drain the hot streams into cold segments, durably and
crash-safely, in the background.

**Concepts.**

- **Consumer group drain.** Reads new entries from every device's stream via the
  shared group, batching by `max_batch_entries` / `max_batch_bytes` /
  `flush_interval` — whichever trips first.
- **Ack-after-durable ⇒ at-least-once.** A batch is written as one segment; only
  after the segment is durable are the stream entries `XACK`ed. A crash before
  ack means the entries are re-read next time (duplicates), which the query
  layer deduplicates — turning at-least-once storage into exactly-once
  observation.
- **Backpressure without loss.** If a flush fails (bucket outage), the batch is
  *retained* and retried with backoff; the archiver stops reading more so memory
  stays bounded while the backlog waits safely in Redis.
- **Crash recovery.** On startup `reclaim` runs `XAUTOCLAIM` to adopt entries a
  previous run read but never acked.
- **Clean shutdown.** On `ctx` cancellation, `finalFlush` runs with a *fresh*
  context (with timeout) so the drain isn't aborted by the very cancellation
  that triggered it — this is what the C++ supervisor's graceful `stop()` relies
  on.

---

## 12. Query engine — `internal/query`

**File:** `engine.go`.

**Responsibility.** Present the hot and cold tiers as one consistent view, and
implement verification and audit reporting over it.

**Concepts.**

- **Tier merge + dedupe + sort.** `Query` prunes cold manifests by time/device,
  reads matching segments, merges with the hot tier, **deduplicates by
  `entry_id` (ULID)**, applies filters (device, severity floor, subsystem,
  trace, time window), sorts by ingest time (ULID as tie-break), and caps at
  `max_results`. Dedup-by-id is what makes at-least-once archival safe to read.
- **`VerifyRange`.** Re-runs `VerifyEntry` on every entry (recompute hash +
  check signature) and checks each `prev_hash` against the previous entry's
  hash. Any break is reported with the exact `entry_id` and reason. The first
  entry's back-link points outside the window and is not checked.
- **`AuditReport`.** Collects a job's entries by `trace_id`, verifies each,
  hashes the ordered entry hashes into a `report_hash`, and Ed25519-signs it —
  the exported bundle is itself tamper-evident and is the raw material for a
  Certificate of Sanitization.
- **Documented simplification.** Collect-then-sort with a server cap, rather
  than a streaming cross-tier merge-sort — the right trade-off at fleet-log
  scale; the streaming gRPC contract already permits a smarter engine later
  without breaking clients.

---

## 13. Observation transport — `internal/grpcapi`

**Files:** `server.go`, `service.go`.

**Responsibility.** Expose `LogService` over TLS gRPC with a uniform
interceptor chain.

**Concepts.**

- **Interceptor chain (middleware).** Both unary and stream RPCs pass through
  `recover → authorize → meter`, applied uniformly so a new RPC can't forget
  auth or panic-safety:
  - `recover` converts a handler panic into `codes.Internal` (and logs it)
    instead of crashing the server.
  - `authorize` reads `authorization: Bearer <token>` metadata and calls
    `Manager.Authorize(..., FeatureQuery)`; missing/invalid ⇒
    `codes.Unauthenticated`.
  - `meter` records per-method, per-status counters.
- **Server streaming.** `Query` and `Tail` return `stream LogEntry`. `Tail`
  subscribes to the hub and forwards matching entries until the client's
  `ctx` is done. `Query` streams the engine's result set.
- **Input validation** at the edge: `VerifyRange` requires a device;
  `ExportAuditReport` requires a trace and returns `codes.NotFound` for an empty
  window — errors are mapped to precise gRPC status codes.
- **TLS/mTLS** via `credentials.NewTLS`, same config semantics as the MQTT
  listener.

---

## 14. Command-line tools — `cmd/*`, `tools/gencerts`

- **`cmd/devlogd`** — the service (see §2).
- **`cmd/logctl`** — the operator CLI and reference gRPC client:
  `query | tail | verify | export | stats`, plus `sim`, an MQTT producer that
  emits a realistic sanitization job (the Go reference for the C++ producer and
  the language clients). Uses a `PerRPCCredentials` bearer that *refuses to run
  without transport security*, so the token never travels in the clear.
- **`cmd/lictl`** — the license authority CLI: `keygen` (Ed25519 keypairs, priv
  `0600`), `issue` (mint a signed `.lic` with subject/features/validity/
  max-sessions), `inspect` (decode + verify a license). `issuer.key` is the
  crown jewel and is kept off the deployment host.
- **`cmd/licensed`** — the mini license server for online mode (TLS,
  `/v1/activate`, `/v1/heartbeat`, `/healthz`).
- **`tools/gencerts`** — dev PKI generator (CA + server/client certs with the
  right SANs). Swap in real PKI for production by pointing config at other PEMs.

---

## 15. Failure behaviour

Every component fails independently and loudly rather than silently — see
[`DESIGN.md`](DESIGN.md) §5 for the full failure-mode table (trigger,
behaviour, and data at risk) covering `config`, `broker`, `ingest`,
`hot`/`cold` storage, `archive`, `license`, and `query`.
