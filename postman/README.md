# DeviceLogger gRPC Postman collection

Integration-testing assets for the **devlogd** observation plane: the gRPC service
`devicelog.v1.LogService` defined in [`api/proto/devicelog/v1/query.proto`](../api/proto/devicelog/v1/query.proto)
(which imports [`log.proto`](../api/proto/devicelog/v1/log.proto)).

The collection covers **all five** methods:

| Method | Kind | Purpose |
| --- | --- | --- |
| `Query` | server-streaming | Historical query, merged across Redis + object store |
| `Tail` | server-streaming (unbounded) | Live follow of new entries |
| `VerifyRange` | unary | Re-verify signatures + hash chain for a device |
| `ExportAuditReport` | unary | Signed, tamper-evident report for one job |
| `GetStats` | unary | Per-device hot-tier counts + last ingest |

Files in this directory:

- `DeviceLogger.postman_collection.json` - the collection (schema v2.1).
- `DeviceLogger.postman_environment.json` - environment variables.
- `run-integration.sh` - `grpcurl`-based CI runner (Newman can't do gRPC - see below).

---

## 1. Prerequisites

Bring up devlogd and its dependencies (Redis + MinIO), then run the service:

```bash
docker compose -f deploy/docker-compose.yml up -d redis minio
go run ./cmd/devlogd
```

Ingest at least one job so `Query`, `VerifyRange`, and `ExportAuditReport`
return data (any client works). For example, using the bundled simulator:

```bash
go run ./cmd/logctl sim -device station-01 -license station-01.lic
```

Note the sanitization job / trace id it produces - you will put it into the
`traceId` variable so `Query` and `ExportAuditReport` target a real job.

---

## 2. Import into the Postman app

1. Open Postman -> **Import**.
2. Select both `DeviceLogger.postman_collection.json` and
   `DeviceLogger.postman_environment.json`.
3. In the environment selector (top right), choose **DeviceLogger (local devlogd)**.

Postman gRPC requests reference the proto contract. If Postman prompts for the
proto, point it at `api/proto/devicelog/v1/query.proto` with the import path
`api/proto` (so `import "devicelog/v1/log.proto"` resolves). The collection
records these under each request's `config` and as collection variables
`protoFile` / `protoImportPath`.

---

## 3. Configure TLS (CA certificate) in Postman

The server dials over TLS at `localhost:9443`; its certificate SAN includes
`localhost`. A Postman collection **cannot embed a CA file**, so add it once in
the app:

1. **Settings -> Certificates -> CA Certificates** -> enable, then select
   [`deploy/certs/ca.crt`](../deploy/certs/ca.crt).
2. Keep `grpcUrl` host as `localhost` so it matches the cert SAN. (If you must
   dial by IP/alias, set the request's TLS **server name** to `localhost`.)

**Mutual TLS (only if your deployment requires it):** add a client certificate
under **Settings -> Certificates -> Client Certificates** for host
`localhost:9443`, using [`deploy/certs/client.crt`](../deploy/certs/client.crt)
and `deploy/certs/client.key`.

---

## 4. Load the license token

Every RPC sends metadata `authorization: Bearer {{licenseToken}}`. The token is
the **single line** in `operator.lic` (repo root) - a `query`-feature license.

Copy it into the `licenseToken` environment variable:

```bash
tr -d '\n' < operator.lic
```

Paste the output into `licenseToken` (Environments -> DeviceLogger -> Current
Value). Without it, every RPC fails with `UNAUTHENTICATED`. Do not commit the
token - the environment ships with `licenseToken` empty.

Other variables: `grpcUrl` (`localhost:9443`), `deviceId` (`station-01`),
`traceId` (set to the real job id from step 1).

> `Tail` is unbounded - it streams until you cancel it in the Postman UI.

---

## 5. CI / automation: Newman does NOT support gRPC

**Newman (the Postman CLI runner) only executes HTTP requests. It cannot run
gRPC requests.** So the collection above is for **manual/interactive** testing
in the Postman app, and it cannot be run headless in CI.

For automatable integration testing use the provided script, which exercises the
same five methods over the same TLS + bearer contract using
[`grpcurl`](https://github.com/fullstorydev/grpcurl):

```bash
postman/run-integration.sh
```

It:

- dials `localhost:9443` with `-cacert deploy/certs/ca.crt -servername localhost`,
- sends `-H "authorization: Bearer $(tr -d '\n' < operator.lic)"`,
- loads the proto with `-import-path api/proto -proto api/proto/devicelog/v1/query.proto`,
- calls `GetStats`, `Query`, `Tail` (time-bounded), `VerifyRange`,
  `ExportAuditReport`, and returns non-zero if any RPC errors.

Override defaults via environment variables, e.g.:

```bash
DEVICE_ID=station-02 TRACE_ID=job-01JZ... postman/run-integration.sh
```

Requires `grpcurl` on `PATH` and a running devlogd (section 1).
