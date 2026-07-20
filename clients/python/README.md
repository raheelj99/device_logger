# devlog Python clients

Python clients for the `devlogd` device-logging service, mirroring the verified
Node reference client in `clients/node/`. Same connection contract, one schema,
no drift:

- **Ingest** (write path): MQTT over TLS on `:8883`, `username = device id`,
  `password = ingest license token`, payload = serialized `devicelog.v1.LogEntry`.
- **Observe** (read path): gRPC over TLS on `:9443`, `authorization: Bearer
  <query license token>`, service `devicelog.v1.LogService`.

## Layout

```
devlog_client/        shared, importable package (the real client)
  config.py           env-driven configuration
  entry.py            pure LogEntry / sanitization-job builders
  publisher.py        MQTT/TLS Publisher (paho-mqtt)
  observer.py         gRPC/TLS Observer (grpcio)
  service.py          framework-agnostic JSON service layer
  cli.py              `python -m devlog_client.cli ...`
  gen/                generated protobuf stubs (from clients/proto)
scripts/gen_proto.sh  regenerate the stubs
django_app/           Django REST wrapper  (manage.py runserver)
flask_app/app.py      Flask REST wrapper
fastapi_app/main.py   FastAPI REST wrapper (uvicorn)
tests/                pytest unit tests (+ one skipped integration test)
```

## Setup

```bash
cd clients/python
python3 -m venv .venv          # or: virtualenv -p python3 .venv
source .venv/bin/activate
pip install -e ".[django,flask,fastapi,test]"
```

## Generate protobuf stubs

The stubs are generated from the shared contract in `clients/proto` and are
committed, but you can regenerate them any time:

```bash
bash scripts/gen_proto.sh
```

## Configuration

All settings default to the local dev deployment and are overridable by env:

| Env var                 | Default                        |
|-------------------------|--------------------------------|
| `DEVLOG_HOST`           | `localhost`                    |
| `DEVLOG_MQTT_PORT`      | `8883`                         |
| `DEVLOG_GRPC_PORT`      | `9443`                         |
| `DEVLOG_DEVICE_ID`      | `station-01`                   |
| `DEVLOG_INGEST_LICENSE` | `<repo>/station-01.lic`        |
| `DEVLOG_QUERY_LICENSE`  | `<repo>/operator.lic`          |
| `DEVLOG_CA_FILE`        | `<repo>/deploy/certs/ca.crt`   |

## Run the unit tests

No server required — these cover message construction and wire encoding:

```bash
pytest                    # 7 unit tests pass, 1 integration test skipped
```

## CLI

Mirrors the Node `index.js` / `logctl`:

```bash
python -m devlog_client.cli publish [trace-id]
python -m devlog_client.cli query   [trace-id]
python -m devlog_client.cli verify  [device]
python -m devlog_client.cli export  <trace-id>
python -m devlog_client.cli stats
python -m devlog_client.cli tail
python -m devlog_client.cli e2e      # publish then query/verify/export
```

## Run the framework apps

Each exposes the same REST surface over the shared client:
`POST /jobs`, `GET /entries?trace=...`, `GET /verify/<device>`,
`GET /report/<trace>`, `GET /stats`.

```bash
# Flask
python flask_app/app.py                                  # :8081

# FastAPI
uvicorn main:app --app-dir fastapi_app --port 8082       # :8082

# Django
python django_app/manage.py runserver 0.0.0.0:8083       # :8083
```

## End-to-end (needs a live deployment)

The full e2e (`cli e2e` and the `@pytest.mark.integration` test) requires
`devlogd` plus its backing Redis and MinIO to be running. That normally comes up
via `deploy/docker-compose.yml`; **Docker is not available in this sandbox**, so
the integration test is skipped by default. To run it against a real deployment:

```bash
pytest --integration        # or: pytest -m integration
```
