# Device Logger — Operator & Integrator Manual

devlogd is a single-binary logging service: your C++ sanitization stations
publish structured log entries over **MQTT/TLS**; the service signs and chains
every entry, keeps a hot window in **Redis**, archives compressed segments to
an **S3 bucket (MinIO)**, and serves queries, live tails, and signed audit
reports over **gRPC/TLS**. Grafana + Prometheus provide the dashboard.

```
C++ station ──MQTT 8883 (TLS)──▶ devlogd ──▶ Redis (hot, 24h)
                                   │  └────▶ MinIO (cold, zstd segments)
operator ────gRPC 9443 (TLS)──────▶│
Prometheus ──HTTP 9090 /metrics ──▶│           Grafana :3000
```

---

## 1. Prerequisites

| Tool           | Needed for                  | Install                                   |
| -------------- | --------------------------- | ----------------------------------------- |
| Go ≥ 1.25      | building the service        | `winget install GoLang.Go`                |
| Docker Desktop | Redis/MinIO/Grafana stack   | already installed                         |
| buf (optional) | regenerating protobuf code  | `go install github.com/bufbuild/buf/cmd/buf@latest` |

Generated protobuf code is committed — you only need buf when changing
`api/proto/**.proto`.

## 2. First-time setup (one time per deployment)

All commands run from the repository root.

```powershell
# 1. Dev PKI: CA + server cert (SANs: localhost, devlogd, licensed, host.docker.internal)
go run ./tools/gencerts

# 2. Keypairs: license issuer + entry-signing key
go run ./cmd/lictl keygen -name issuer
go run ./cmd/lictl keygen -name signing

# 3. Licenses: one per station (ingest) + one for operators (query)
go run ./cmd/lictl issue -subject station-01 -features ingest -max-sessions 2
go run ./cmd/lictl issue -subject "*" -features query -out operator.lic

# 4. Service config
copy config\devlogd.example.yaml config\devlogd.yaml

# 5. Backing services
docker compose -f deploy/docker-compose.yml up -d
```

Secrets hygiene: `deploy/certs/`, `deploy/keys/`, and `*.lic` are gitignored.
`issuer.key` is the crown jewel — whoever holds it can mint licenses. In
production keep it off the deployment machine entirely (issue licenses
elsewhere; devlogd only needs `issuer.pub`).

### 2a. Self-hosting Redis/MinIO (single-station deployments)

Step 5 above (backing services) assumes Docker. On a station appliance
without a separately managed Redis/MinIO, set `redis.auto_start.enabled: true`
and/or `s3.auto_start.enabled: true` in `config/devlogd.yaml` (both are `false`
by default — the pre-existing fail-fast behavior for anyone already pointing
at externally managed instances is unchanged unless you opt in). With it on,
devlogd spawns `redis-server`/`minio` itself at startup — bound to `redis.addr`
/ `s3.endpoint`, data persisted under `redis.auto_start.data_dir` /
`s3.auto_start.data_dir` (default `data/redis`, `data/minio`, gitignored) —
waits for it to become ready (`auto_start.timeout`, default 15s), and stops it
gracefully on shutdown. This requires the `redis-server`/`minio` binaries to be
installed on the host (see the `.deb` dependency list in
[`deploy/embed/README.md`](../deploy/embed/README.md), which already assumes
both are co-located with devlogd on the appliance).

This is a **startup-time convenience, not a runtime supervisor**: if Redis or
MinIO die while devlogd is already running, devlogd does not try to respawn
them — the existing failure-mode behavior in [`DESIGN.md`](DESIGN.md) §5 still
applies. If devlogd only *connected* to an already-running instance (auto-start
never spawned anything), it never signals that instance on shutdown.

## 3. Running

```powershell
go run ./cmd/devlogd -config config/devlogd.yaml     # natively (dev)
# or containerized:
docker compose -f deploy/docker-compose.yml --profile service up -d --build
```

Startup is fail-fast: bad config, unreachable Redis/MinIO, or missing keys
abort with a precise error. When up:

| Endpoint                      | What                                  |
| ----------------------------- | ------------------------------------- |
| `mqtts://localhost:8883`      | ingestion (MQTT over TLS)             |
| `localhost:9443`              | gRPC LogService (TLS)                 |
| `http://localhost:9090/metrics` | Prometheus metrics                  |
| `http://localhost:9090/healthz` / `/readyz` | liveness / readiness    |
| `http://localhost:3000`       | Grafana (admin/admin) → "Device Logger" dashboard |
| `http://localhost:9001`       | MinIO console (minioadmin/minioadmin) |

Smoke test without the C++ app:

```powershell
go run ./cmd/logctl sim -device station-01 -license station-01.lic
go run ./cmd/logctl tail -license operator.lic          # in a second terminal
```

## 4. Configuration reference (`config/devlogd.yaml`)

| Key | Default | Meaning |
| --- | --- | --- |
| `mqtt.listen` | `:8883` | MQTT/TLS listener |
| `mqtt.tls.cert_file` / `key_file` | — | server certificate/key (PEM) |
| `mqtt.tls.client_ca_file` | off | set to require client certificates (mutual TLS) |
| `grpc.listen` | `:9443` | gRPC/TLS listener (same TLS block semantics) |
| `http.listen` | `:9090` | metrics + health |
| `redis.addr` / `password` | `localhost:6379` | hot tier |
| `redis.hot_retention` | `24h` | window kept in Redis; older entries live only in the bucket |
| `redis.auto_start.*` | disabled | spawn a local `redis-server` at startup if `addr` is unreachable — §2a |
| `s3.endpoint`, `access_key`, `secret_key`, `bucket`, `use_tls` | MinIO defaults | cold tier |
| `s3.auto_start.*` | disabled | spawn a local `minio` at startup if `endpoint` is unreachable — §2a |
| `signing.key_file` | — | Ed25519 key that signs every entry |
| `signing.key_id` | `devlogd-2026` | key name recorded per entry (rotation, §8) |
| `license.issuer_pub_file` | — | public key licenses must be signed by |
| `license.mode` | `offline` | `online` adds activation against `licensed` |
| `license.server_url` / `server_ca_file` | — | license server endpoint + CA (online mode) |
| `license.grace` | `72h` | how long known-good sessions survive an unreachable license server |
| `archive.flush_interval` | `60s` | max time an entry waits before archiving |
| `archive.max_batch_bytes` / `max_batch_entries` | 8 MiB / 5000 | segment size targets |
| `query.max_results` | `10000` | server-side result cap |
| `query.default_lookback` | `168h` | window when a query has no `from` |
| `log.level` | `info` | devlogd's own log level |

Any value can be overridden by environment (container-friendly):
`DEVLOG_MQTT_LISTEN`, `DEVLOG_GRPC_LISTEN`, `DEVLOG_HTTP_LISTEN`,
`DEVLOG_REDIS_ADDR`, `DEVLOG_REDIS_PASSWORD`, `DEVLOG_S3_ENDPOINT`,
`DEVLOG_S3_ACCESS_KEY`, `DEVLOG_S3_SECRET_KEY`, `DEVLOG_S3_BUCKET`,
`DEVLOG_LICENSE_MODE`, `DEVLOG_LICENSE_SERVER_URL`, `DEVLOG_LOG_LEVEL`.

## 5. Licensing

The **license file is the credential**. `lictl issue` produces a single-line
base64 token, Ed25519-signed by the issuer key:

- MQTT: `username = device_id`, `password = token`
- gRPC: `authorization: Bearer <token>` metadata (`logctl` does this for you)

Checks enforced at session start: signature, validity window (±5 min clock
skew), subject binding (`subject` must equal the device id, or be `*`),
feature grant (`ingest` for MQTT, `query` for gRPC).

**Offline mode** (default): verification is purely local — right for
air-gapped labs and field robots.

**Online mode** (`license.mode: online`): devlogd additionally POSTs
`/v1/activate` to the license server at session start. The server re-verifies
and enforces `max_sessions` per license across devices. If the server is
*unreachable*, sessions that have activated successfully before continue for
`license.grace` (default 72 h); a *definitive rejection* always denies.
Run the server with:

```powershell
docker compose -f deploy/docker-compose.yml --profile license up -d --build
# or natively: go run ./cmd/licensed
```

Useful commands:

```powershell
go run ./cmd/lictl inspect station-01.lic          # decode + verify a license
go run ./cmd/lictl issue -subject station-02 -features ingest -days 90
```

## 6. Integrating the C++ application

Full working example: `examples/cpp/publisher/` (paho-mqtt-cpp + protobuf).
The contract:

1. **Schema**: generate C++ from `api/proto/devicelog/v1/log.proto`
   (`protobuf_generate` in the example's CMakeLists). The same file generates
   the Go side — one schema, no drift.
2. **Connect**: `ssl://<host>:8883`, trust store = your deployment's `ca.crt`,
   username = device id, password = license token.
3. **Publish** to `devlog/v1/<device_id>/<subsystem>` with QoS 1, payload =
   serialized `LogEntry`.
4. **Fill**: `device_id` (must match username), `seq` (monotonic per device),
   `device_time`, `severity`, `subsystem`, `message`, `trace_id` (= your
   sanitization job id), and `sanitization` for job events (`attributes` for
   anything else).
5. **Never fill**: `entry_id`, `ingest_time`, `audit` — server-owned; the
   server also rejects entries whose `device_id` differs from the session's.

Sanitization events carry: target media (serial/model/capacity/type),
standard (`NIST_800_88_{CLEAR,PURGE,DESTROY}`, `IEEE_2883_*`), technique,
phase (`STARTED → PROGRESS → VERIFYING → COMPLETED|FAILED|ABORTED`),
progress, verification (method/sample %/passed), operator id.

**Extending the schema** stays backward compatible if you: only add fields
with fresh numbers, never reuse or renumber, and `reserve` removed numbers.
Old devlogd versions carry unknown fields through untouched — they even remain
inside the signed hash.

## 7. Observing: queries, tails, audits

```powershell
go run ./cmd/logctl query  -license operator.lic -since 1h -severity error
go run ./cmd/logctl query  -license operator.lic -trace job-01JZ... -limit 500
go run ./cmd/logctl tail   -license operator.lic -device station-01
go run ./cmd/logctl stats  -license operator.lic
go run ./cmd/logctl verify -license operator.lic -device station-01 -since 24h
go run ./cmd/logctl export -license operator.lic -trace job-01JZ... -out job.report.json
```

- `query` merges Redis and the bucket transparently and deduplicates.
- `verify` re-checks every signature and chain link; any tampering (edit,
  deletion, reordering) is reported with the exact entry where the chain breaks.
- `export` produces the **signed audit report** for one sanitization job — the
  machine-readable basis for a Certificate of Sanitization. The report embeds
  every entry, per-entry verification, and a report-level Ed25519 signature.

Any gRPC client works the same way (grpcurl, C++, Python): TLS + bearer
metadata; the API is `devicelog.v1.LogService` in `api/proto/devicelog/v1/query.proto`.

## 8. Runbook

**Hot retention / sizing.** Redis holds `hot_retention` of data per device.
Rough sizing: entries/s × avg entry bytes × retention seconds. Reduce
retention or add Redis memory; cold queries are unaffected.

**Key rotation (signing).** Generate a new key (`lictl keygen -name signing-2027`),
set `signing.key_file` + a *new* `signing.key_id`, restart. Old entries verify
against the old key — keep retired *public* keys and add them to the verifier
set (see `verifier.Add` in `cmd/devlogd/main.go`) so `verify`/`export` keep
validating history.

**License rotation.** Issue a new `.lic`, swap the file on the station,
reconnect. Revocation in offline mode = short validity windows; in online
mode the server is the enforcement point.

**Backup.** The bucket is the system of record: back up MinIO's volume or
replicate the bucket (`segments/` + `manifests/`). A graceful stop (SIGTERM,
`systemctl stop`, Ctrl-C) drains the *entire* hot-tier backlog to cold storage
before devlogd exits, not just whatever the archiver had already batched — so
a planned restart never leaves anything un-archived. Only a hard crash (no
chance to run shutdown, e.g. `kill -9`, power loss) risks losing the
un-archived tail, bounded by `archive.flush_interval`, and even then only if
the bucket flush also failed; Redis AOF persistence (on in compose) makes that
tail recoverable via the consumer-group reclaim on the next startup regardless.

**Crash recovery** is automatic: unarchived entries are re-claimed from the
Redis consumer group on restart; duplicate archiving is absorbed by
query-time deduplication.

**Troubleshooting.**

| Symptom | Check |
| --- | --- |
| MQTT connect refused | `lictl inspect <lic>`: expiry, subject = username, `ingest` feature; server log says why |
| TLS handshake failure | client trusts `ca.crt`? host matches a SAN? (`gencerts -hosts` to add) |
| gRPC `Unauthenticated` | license lacks `query` feature, or missing `authorization` metadata |
| entries in `tail` but not Grafana | Prometheus target down — check `http://localhost:9091/targets` |
| `readyz` 503 | startup incomplete — Redis/MinIO reachable? |
| verify reports breaks | treat as an incident: identify the window, export reports, investigate storage access |

**Upgrades.** Single binary: build, stop (graceful — drains archiver), start.
Proto changes must follow §6's compatibility rules.
