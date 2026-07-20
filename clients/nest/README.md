# devlog NestJS client

A NestJS client for the `devlogd` logging service. It provides:

- a small **REST API** wrapping devlogd's two planes (MQTT/TLS ingest, gRPC/TLS
  observation), and
- a **CLI smoke test** (`e2e`) that publishes a sanitization job and reads it
  back end-to-end.

It speaks the exact same connection contract as the reference Node client
(`clients/node`) and loads the shared protobuf schema from `clients/proto`.

## Install

```bash
cd clients/nest
npm install
```

## Unit tests (no infra required)

```bash
npm test
```

The unit tests (`src/devlog/entry.spec.ts`) exercise the pure builders and the
protobuf codec against the shared `.proto` — no devlogd, Redis, or MinIO needed.

## REST app

```bash
npm run start          # ts-node, port 3001 (override with PORT)
# or
npm run build && npm run start:prod
```

Endpoints:

| Method | Path                        | Purpose                                  |
| ------ | --------------------------- | ---------------------------------------- |
| POST   | `/jobs`                     | publish a sanitization job, return trace |
| GET    | `/entries?traceId=&since=`  | query historical entries (since = ms)    |
| GET    | `/verify/:deviceId`         | verify the per-device hash chain (24h)   |
| GET    | `/report/:traceId`          | export the signed audit report for a job |
| GET    | `/stats`                    | per-device hot-tier counts               |

## End-to-end smoke test (needs a live stack)

The live `e2e` requires devlogd + Redis + MinIO. From the repo root:

```bash
docker compose -f deploy/docker-compose.yml up -d redis minio
go run ./cmd/devlogd
```

Then, from `clients/nest`:

```bash
npm run e2e
```

It publishes a job over MQTT, waits ~1s, then queries, verifies, and exports it
over gRPC. Exits non-zero on failure.

## Configuration

Everything is env-overridable (defaults target a local dev deployment):

`DEVLOG_HOST`, `DEVLOG_MQTT_PORT`, `DEVLOG_GRPC_PORT`, `DEVLOG_DEVICE_ID`,
`DEVLOG_INGEST_LICENSE`, `DEVLOG_QUERY_LICENSE`, `DEVLOG_CA_FILE`,
`DEVLOG_PROTO_DIR`, and `PORT` (REST listen port).

Credentials are files, never inline: the ingest license (default
`station-01.lic`) is the MQTT password; the query license (default
`operator.lic`) is the gRPC `Bearer` token.
