# Elite Engineering — the What, Why & How

This document is the reasoning layer behind the code. [`DESIGN.md`](DESIGN.md)
catalogs *which* patterns are used; this explains the *mentality* that chooses
them, and shows how the deployment, multi-language clients, and test/integration
work added around the service embody the same discipline.

The thesis of "elite" here is narrow and testable: **every decision is
deliberate, every claim is verifiable, and every failure mode is named before
it happens.** Cleverness is not the goal — control is.

---

## 1. The five principles

| Principle | What it means in practice | Where you can see it |
| --- | --- | --- |
| **Contracts before code** | One schema (`api/proto`) generates the Go service *and* every client — C++, Node, Nest, Python. Drift is impossible, not merely discouraged. | `clients/proto` is the same `.proto` the server compiles; client unit tests assert a byte round-trip through it. |
| **Fail fast, fail loud** | Bad config, unreachable dependencies, or a broken chain abort at the earliest possible point with a precise message — never a silent wrong answer. | `config.validate()`, devlogd's startup pings, `DevlogdSupervisor::start()` detecting an early child exit, checksum/chain verification. |
| **Trust is earned per entry** | Security is not a perimeter; it is a property carried by each record: signed, hash-chained, license-gated, TLS-only. | `internal/sign`, per-session license auth, mutual-TLS option. |
| **Every claim is verifiable** | Nothing is asserted that a test or a runtime probe cannot confirm — including "it works from another language". | Go `-race` tests, client codec round-trip tests, the cross-language e2e harness, `/readyz`. |
| **Name the failure first** | Each component's failure mode and blast radius is documented and bounded *before* it is relied upon. | `DESIGN.md` §5 failure table; the supervisor's fail-fast policy note; drop-on-slow tail. |

Everything below is an application of one of these five.

---

## 2. What was built, and why each choice

### 2.1 Deployment — C++ spawns devlogd (`deploy/embed/`)

**What.** A dependency-free `DevlogdSupervisor` (POSIX + libstdc++ only) that your
C++ application uses to launch devlogd during init, block until it is actually
ready, and drain it gracefully on shutdown.

**Why this shape.**
- *Lifecycle coupling was the requirement*: the audit logger must live and die
  with the sanitization app, on hosts that may not run systemd (field robots,
  minimal containers). So the app owns the process — not systemd.
- *Readiness, not liveness*: `start()` gates on `GET /readyz == 200`, not on
  "the process exists". A logger that is up but can't reach Redis is useless;
  we refuse to proceed until it can actually accept work. This is *fail fast*
  applied to a dependency you can't sanitize media without.
- *Graceful drain*: `stop()` sends `SIGTERM` (devlogd flushes the archiver),
  waits a bounded grace window, then escalates to `SIGKILL`. Bounded, never
  hanging — *name the failure first* (a wedged child) and cap it.
- *Zero third-party deps*: the readiness probe is a raw-socket HTTP GET rather
  than linking libcurl. Fewer dependencies on the appliance = smaller attack
  surface and simpler packaging. *Least surface* extends to the integration glue.

**How it's proven.** It compiles under `-std=c++17` and was runtime-tested
against a fake devlogd: spawn → readiness-gate → SIGTERM graceful stop, all
observed. The `.deb` guidance encodes the security boundary as file permissions
(signing key `0600`, dedicated `devlog` user, `issuer.key` never shipped).

### 2.2 Clients in five stacks — Node, Nest, Django, Flask, FastAPI

**What.** Each client exercises *both planes*: it publishes a full
sanitization job over MQTT/TLS (ingest) and reads it back over gRPC/TLS
(query, verify chain, export the signed audit report).

**Why both planes.** A client that only queries proves nothing about ingestion,
and vice-versa. The headline test — `e2e` — is the real contract: *write from a
non-Go client, read it back, and confirm the signatures the Go service produced
still verify.* That is the only test that proves the module is genuinely
polyglot-ready.

**Why a shared, verified reference.** The Node client was built and tested
first, then used as the exact template for the others. This is *contracts
before code* at the process level: one reference, mirrored, so five clients
can't quietly diverge in how they authenticate, name topics, or encode entries.

**How correctness is anchored.** The most drift-prone thing in any client is
message encoding. So every client's unit tests do a **protobuf round-trip
through the shared `.proto`** and assert field numbers/types — the same bytes
the Go server will parse. These tests need no server, Redis, or MinIO, so they
run anywhere (CI, a laptop, a locked-down build box) and they run fast.

**The one thing every client refuses to do.** Set `entry_id`, `ingest_time`,
or `audit`. Those are server-owned; the pipeline overwrites them and rejects a
device that lies about its identity. The clients encode that boundary in a
builder that structurally cannot populate those fields (tested), so the
*trust-is-earned-per-entry* rule holds no matter which language calls in.

### 2.3 Tests — filling the Go coverage gaps

**What.** New Go unit tests for the previously-untested packages that carry the
most risk: `config` (fail-fast validation), `ingest` (the sign-and-chain
critical section), `query` (tier merge, dedupe, tamper detection), and
`grpcapi` (auth interceptor + RPC validation).

**Why these four.** Coverage is not uniform in value. These packages are where
a silent bug would be *most expensive*: a config that loads when it shouldn't,
an ingest path that breaks the chain, a query that returns tampered data as
clean, or an RPC that skips the license check. Each test targets a specific
failure mode, not a line-count.

**How they stay honest.**
- They use `miniredis` and an in-memory object store — real behavior, no
  infrastructure, so they're deterministic and CI-friendly.
- The security-critical ones assert *negative* space: ingest **rejects** an
  identity mismatch and **overwrites** a producer-forged audit block; query
  **detects** a post-signing content tamper; the interceptor **rejects**
  missing and malformed tokens. Proving the system says "no" matters more than
  proving it says "yes".
- They pass under `-race` — the ingest chain mutex and hub fan-out are
  concurrency-sensitive, so the race detector is part of the claim.

### 2.4 Integration — Postman (`postman/`)

**What.** A Postman gRPC collection for all five `LogService` methods (TLS +
bearer license), an environment, and — because **Newman cannot drive gRPC** — a
`grpcurl` script (`run-integration.sh`) that automates the same five calls in
CI.

**Why call out the Newman limitation loudly.** *Name the failure first.* A
collection that silently can't run in CI is a trap. Documenting that Postman's
gRPC UI is manual, and shipping the `grpcurl` path for automation, is the
difference between an honest integration story and a demo.

---

## 3. How to verify every claim yourself

Elite work is auditable. Each claim in this repo maps to a command:

```bash
# Go: build, vet, unit + race
go build ./... && go vet ./... && go test -race ./...

# C++ supervisor: build the example
cmake -S deploy/embed -B build/embed && cmake --build build/embed

# Node / Nest client unit tests (no infra needed)
(cd clients/node && npm install && npm test)
(cd clients/nest && npm install && npm test)

# Python client unit tests (no infra needed)
(cd clients/python && python3 -m venv .venv && . .venv/bin/activate \
   && pip install -e . && ./scripts/gen_proto.sh && pytest -m "not integration")

# Full cross-language end-to-end (needs Docker):
docker compose -f deploy/docker-compose.yml up -d redis minio
go run ./cmd/devlogd -config config/devlogd.yaml &     # in one shell
(cd clients/node && npm run e2e)                        # publish (MQTT) + read back (gRPC)
```

The live e2e is the capstone: it proves a non-Go client can write an entry the
Go service signs, and read it back with the signature intact.

---

## 4. Deliberate limits (stated, not hidden)

Honesty about scope is part of the discipline:

- **Unit tests over live e2e in CI-by-default.** The cross-language e2e needs
  Redis + MinIO + devlogd; it's gated behind Docker and run explicitly, while
  the codec/contract tests run everywhere. This keeps the fast feedback loop
  infra-free without pretending the e2e is automatic.
- **Postman gRPC is manual.** By tool limitation, not choice — hence the
  `grpcurl` CI companion.
- **One devlogd writer.** Inherited from the service's design (per-device hash
  chain assumes a single writer); the supervisor spawns exactly one. Sharding
  is the documented scaling path.
- **Clients are reference integrations, not published SDKs.** They are complete
  and correct against the contract, but versioning/packaging for distribution
  is left to the consuming team.

Nothing here is accidental. That is the whole point.
