# devlogd clients

Reference clients that exercise **both planes** of devlogd from four language
stacks. Each one publishes a full media-sanitization job over **MQTT/TLS**
(ingest) and reads it back over **gRPC/TLS** (query, verify the hash chain,
export the signed audit report) — proving the service is genuinely polyglot.

| Stack | Path | Unit tests | Run |
| --- | --- | --- | --- |
| Node.js | [`node/`](node/) | `npm test` (7) | `npm run e2e` |
| NestJS | [`nest/`](nest/) | `npm test` (7) | `npm run e2e`, REST on `:3001` |
| Python — Django | [`python/django_app`](python/django_app) | `pytest` | `python manage.py runserver` |
| Python — Flask | [`python/flask_app`](python/flask_app) | `pytest` | `flask --app app run` |
| Python — FastAPI | [`python/fastapi_app`](python/fastapi_app) | `pytest` | `uvicorn main:app` |

The three Python frameworks are thin REST wrappers over one shared
`devlog_client` package, so the connection logic lives in exactly one place.

## The shared contract (identical in every client)

The single source of truth is [`proto/devicelog/v1/`](proto/devicelog/v1/) — the
same `.proto` files the Go service is generated from.

**Ingest — MQTT/TLS**
- URL `mqtts://<host>:8883`, trust `deploy/certs/ca.crt`, TLS server name `localhost`.
- `username` = device id (`station-01`); `password` = the trimmed contents of an
  `ingest`-feature license (`station-01.lic`).
- Topic `devlog/v1/<device_id>/<subsystem>`; payload = a serialized
  `devicelog.v1.LogEntry`; QoS 1.
- Never set `entry_id`, `ingest_time`, or `audit` — the server owns them and
  rejects a device that publishes under another identity.

**Observe — gRPC/TLS**
- Target `<host>:9443`, trust `deploy/certs/ca.crt`, SNI override `localhost`.
- Metadata `authorization: Bearer <token>` where the token is a `query`-feature
  license (`operator.lic`).
- Service `devicelog.v1.LogService`: `Query`, `Tail` (server-streaming),
  `VerifyRange`, `ExportAuditReport`, `GetStats`.

Everything is env-overridable: `DEVLOG_HOST`, `DEVLOG_MQTT_PORT`,
`DEVLOG_GRPC_PORT`, `DEVLOG_DEVICE_ID`, `DEVLOG_INGEST_LICENSE`,
`DEVLOG_QUERY_LICENSE`, `DEVLOG_CA_FILE`, `DEVLOG_PROTO_DIR`.

## Unit tests need no infrastructure

Every client's unit tests run without devlogd, Redis, or MinIO. They exercise
the drift-prone parts — message construction and a **protobuf round-trip through
the shared `.proto`** — so field numbers and types are asserted against the exact
bytes the Go server parses. Fast, deterministic, CI-friendly.

## The live end-to-end test (needs Docker)

The capstone proves a non-Go client can write an entry the Go service signs, and
read it back with the signature intact:

```bash
# from the repo root — one-time setup already done if these files exist:
#   deploy/certs/*, deploy/keys/*, station-01.lic, operator.lic
docker compose -f deploy/docker-compose.yml up -d redis minio
go run ./cmd/devlogd -config config/devlogd.yaml      # leave running in one shell

# then, in another shell, pick any client:
(cd clients/node && npm install && npm run e2e)
(cd clients/nest && npm install && npm run e2e)
(cd clients/python && . .venv/bin/activate && python -m devlog_client.cli e2e)
```

Each `e2e` publishes a job over MQTT, waits briefly for the pipeline to sign and
store it, then queries it back, verifies the chain, and exports the audit
report — all over gRPC. A non-zero exit means the round trip failed.

See each stack's own `README.md` for install and per-command usage, and
[`../postman/`](../postman/) for interactive/`grpcurl` integration testing of
the gRPC plane.
