# Testing & Verification Reference

How devlogd is verified end to end: the Go unit tests, the multi-language client
harness, the C++ supervisor test, and the Postman/grpcurl integration path —
what each one covers, the concepts and tools behind it, and how to run it.

Companion to [`COMPONENTS.md`](COMPONENTS.md) (what is being tested) and
[`DESIGN.md`](DESIGN.md) (why it's tested this way).

---

## 1. Strategy — the shape of the test suite

The guiding rule: **fast, deterministic, infra-free tests are the default; the
slow, real-infrastructure end-to-end is explicit and opt-in.**

```
        ▲  few    ┌───────────────────────────────┐
        │         │ live cross-language E2E        │  needs Docker (Redis+MinIO+devlogd)
        │         │ (clients ↔ real devlogd)       │  run explicitly, never in unit CI
        │         ├───────────────────────────────┤
        │         │ integration (Postman / grpcurl)│  needs a running devlogd
   test │         ├───────────────────────────────┤
 volume │         │ Go package tests               │  miniredis + in-memory fakes, `-race`
        │         │ + client contract/unit tests   │  protobuf round-trip, no network
        ▼  many   └───────────────────────────────┘
```

Two principles make this work:

- **Test doubles instead of infrastructure.** Redis is replaced by
  [`miniredis`](https://github.com/alicebob/miniredis) (a pure-Go in-process
  Redis), and the S3 bucket by an in-memory `ObjectStore` fake. The code under
  test is real; only the environment is faked — possible because storage is
  reached through interfaces (ports & adapters, see COMPONENTS §10).
- **Contract round-trip.** The single highest-risk thing in a polyglot system is
  message encoding drift. Every client's unit tests encode a `LogEntry` and
  decode it back through the *same* `.proto` the Go server uses, asserting field
  numbers and types. This catches drift without a server.

---

## 2. Go unit tests — `internal/**/*_test.go`

**Run:** `go test ./...` · with the race detector: `go test -race ./...`

### Concepts

- **Table-driven, black-box-ish package tests.** Standard Go `testing`: files
  named `*_test.go`, functions `func TestXxx(t *testing.T)`, failures via
  `t.Fatal`/`t.Fatalf`. Tests live in the package they test, so they can reach
  unexported helpers (`authorize`, `tailMatch`, `tsOrZero`).
- **`miniredis.RunT(t)`** spins a real Redis API on an ephemeral port for the
  test and is torn down automatically via **`t.Cleanup`** — no Docker, no
  fixture files, no port collisions.
- **In-memory `ObjectStore` fakes** implement the `cold.ObjectStore` interface
  with a `map[string][]byte`, so the archiver and query engine exercise their
  real code paths against a stand-in bucket.
- **Ed25519 test keys** are generated per test with `ed25519.GenerateKey`, so
  signing/verification is real cryptography, not stubbed.
- **Mock gRPC stream.** `mockQueryStream` embeds `grpc.ServerStream` and
  implements `Send`/`Context`, letting a server-streaming RPC be driven and its
  emitted entries captured without a network socket.
- **The race detector (`-race`)** is part of the claim for the
  concurrency-sensitive packages (the ingest chain mutex, the hub fan-out).
- **Isolated metrics.** Each test builds its own `telemetry.NewMetrics()` (a
  fresh Prometheus registry), so there is no cross-test global state or
  double-registration panic.

### What each package's tests assert

| Package | Focus | Representative assertions |
| --- | --- | --- |
| `internal/config` | fail-fast config | defaults applied; `DEVLOG_*` overrides win; missing required field, unknown/`online`-without-URL mode, and bad duration all rejected |
| `internal/ingest` | the sign+chain critical section | entries get a ULID + audit; the hash chain links across entries; **identity mismatch rejected**; non-proto payload rejected; **producer-forged `entry_id`/`audit` overwritten** |
| `internal/query` | tier merge & integrity | hot+cold merge **deduped by ULID** and time-sorted; severity/trace filters; `VerifyRange` clean then **detects a post-signing tamper**; `AuditReport` signed and independently signature-verifiable |
| `internal/grpcapi` | RPC + auth edge | `Query` streams only matching entries; `VerifyRange`/`ExportAuditReport` input validation + `NotFound`; `GetStats`; **interceptor rejects missing and malformed tokens**; `tailMatch`/`tsOrZero` |
| `internal/sign`, `hot`, `cold`, `license` | (pre-existing) crypto, streams, segment round-trip + checksum, license verify | — |

The bias is toward **negative space** — proving the system says *no* (rejects a
lying producer, detects a tamper, refuses a bad token) matters more than proving
the happy path.

---

## 3. The client harness — `clients/`

Five reference clients (Node, NestJS, and Python Django/Flask/FastAPI over a
shared `devlog_client` package) that exercise **both planes** of the service.
This *is* the polyglot integration test: prove a non-Go producer can write an
entry the Go service signs, and read it back with the signature intact.

### The end-to-end flow (`e2e`)

```
publish job (MQTT/TLS, license=password) ─▶ devlogd signs+chains+stores
        │                                            │
        └── wait ~1s for the pipeline ───────────────┘
                     │
   query it back (gRPC/TLS, license=bearer) ─▶ verify chain ─▶ export signed report
```

A non-zero exit means the round trip failed. Each language's `e2e` command
(`npm run e2e`, `python -m devlog_client.cli e2e`) runs this.

### Client unit tests (infra-free)

Each client ships unit tests that need **no** devlogd/Redis/MinIO:

- **Builder correctness** — the sanitization job is the canonical 6-step phase
  sequence (`STARTED → 3×PROGRESS → VERIFYING → COMPLETED`), `seq` is monotonic,
  and the builder *structurally cannot* set the server-owned fields
  (`entry_id`/`ingest_time`/`audit`).
- **Protobuf round-trip through the shared `.proto`** — encode a `LogEntry`,
  decode it independently, assert `device_id`/`seq`/`severity`/`attributes`
  survive with the right field numbers. This is the drift guard.
- **Timestamp conversion** — epoch millis split into `{seconds, nanos}` (the
  `google.protobuf.Timestamp` shape).

Runners: Node `node --test`, Nest `jest` (`ts-jest`), Python `pytest` (the live
e2e test is marked and skipped by default).

### The connection contract every client obeys

- **MQTT ingest:** `mqtts://host:8883`, trust `deploy/certs/ca.crt`, TLS server
  name `localhost`; `username`=device id, `password`=`ingest` license; topic
  `devlog/v1/<device>/<subsystem>`; payload=serialized `LogEntry`; QoS 1.
- **gRPC observe:** `host:9443`, trust the CA, SNI override `localhost`,
  `authorization: Bearer <query-license>`.
- Everything is `DEVLOG_*`-overridable. See [`../clients/README.md`](../clients/README.md).

---

## 4. Deployment test — the C++ supervisor

`deploy/embed/devlogd_supervisor.{hpp,cpp}` launches devlogd as a child of your
C++ app (see COMPONENTS §2 for why). It is verified two ways:

- **Compilation** under `-std=c++17` (`cmake -S deploy/embed -B build/embed`).
- **Runtime behaviour** against a *fake devlogd* (a tiny HTTP server that
  answers `200` on `/readyz` and exits on `SIGTERM`): the test confirms the full
  lifecycle — `start()` spawns and **blocks until readiness**, `running()`
  reports liveness, and `stop()` performs a **graceful SIGTERM drain**. This
  isolates the supervisor's logic (spawn / health-gate / signal handling)
  without needing the real service or its dependencies.

The real integration is then a one-liner: point `binary_path`/`config_path` at
the packaged devlogd (see [`../deploy/embed/README.md`](../deploy/embed/README.md)).

---

## 5. Integration — Postman & grpcurl (`postman/`)

Manual and automated exercise of the gRPC plane against a *running* devlogd.

- **Postman collection** — gRPC requests for all five `LogService` methods, TLS
  with `authorization: Bearer {{licenseToken}}`, example bodies and `pm.test`
  assertions, plus an environment file. CA trust is a one-time manual step
  (Settings → Certificates → add `deploy/certs/ca.crt`) because a collection
  cannot embed a CA.
- **`run-integration.sh` (grpcurl)** — the CI path, because **Newman cannot
  drive gRPC** (it is HTTP-only). The script calls the same five methods over
  TLS with the bearer token loaded at runtime from `operator.lic`, checks exit
  codes, and fails on any RPC error. This keeps gRPC integration automatable
  even though Postman's gRPC UI is interactive.

Full setup: [`../postman/README.md`](../postman/README.md).

---

## 6. Running everything

```bash
# ── Fast, no infrastructure ────────────────────────────────────────────────
go build ./... && go vet ./... && go test -race ./...      # Go: build, vet, unit+race
(cd clients/node   && npm install && npm test)             # Node unit tests
(cd clients/nest   && npm install && npm test)             # Nest unit tests (jest)
(cd clients/python && python3 -m venv .venv && . .venv/bin/activate \
   && pip install -e . && ./scripts/gen_proto.sh && pytest -m "not integration")
cmake -S deploy/embed -B build/embed && cmake --build build/embed   # C++ supervisor

# ── Live, needs Docker (Redis + MinIO + devlogd) ───────────────────────────
docker compose -f deploy/docker-compose.yml up -d redis minio
go run ./cmd/devlogd -config config/devlogd.yaml &         # leave running
(cd clients/node && npm run e2e)                           # cross-language round trip
./postman/run-integration.sh                               # gRPC integration via grpcurl
```

Prerequisites for the live path (already present if these files exist):
`deploy/certs/*`, `deploy/keys/*`, `station-01.lic` (ingest), `operator.lic`
(query). Regenerate with the `Makefile` targets (`make certs keys license
operator-license`) or the commands in [`MANUAL.md`](MANUAL.md) §2.

---

## 7. Coverage matrix — component → how it's verified

| Component | Go unit | Client contract | Live e2e | Integration |
| --- | :--: | :--: | :--: | :--: |
| proto contract | ✓ (via all) | ✓ round-trip | ✓ | ✓ |
| config | ✓ | | | |
| sign (hash/chain/verify) | ✓ | | ✓ (verify/report) | ✓ |
| license auth | ✓ | ✓ (connect) | ✓ | ✓ |
| broker (MQTT/ACL) | | ✓ (publish) | ✓ | |
| ingest pipeline + hub | ✓ | | ✓ | |
| hot (Redis streams) | ✓ | | ✓ | |
| cold (segments/manifest) | ✓ | | ✓ | |
| archive | ✓ (via query dedupe) | | ✓ | |
| query engine | ✓ | ✓ (read back) | ✓ | ✓ |
| grpcapi + interceptors | ✓ | ✓ | ✓ | ✓ |
| C++ supervisor | — (C++ runtime test) | | ✓ (packaged) | |

---

## 8. Known limits (stated deliberately)

- **The live cross-language e2e is not in unit CI** — it needs Redis + MinIO +
  devlogd, so it is gated behind Docker and run explicitly. The
  codec/contract/unit tests run everywhere and are the fast feedback loop.
- **Postman's gRPC UI is manual**; `run-integration.sh` (grpcurl) is the
  automatable equivalent.
- **Clients are reference integrations, not published SDKs** — complete and
  correct against the contract, but not versioned for external distribution.
