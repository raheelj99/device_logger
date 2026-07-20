# Device Logger

Production-grade audit logging for a C++ NIST SP 800-88 / IEEE 2883 media
sanitization application — robotics-ready by design.

One static Go binary (`devlogd`) that ingests structured log entries over
**MQTT/TLS**, signs and hash-chains every entry (**Ed25519**), buffers a hot
window in **Redis Streams**, archives **zstd-compressed, manifest-indexed
segments** to an S3 bucket (**MinIO**), and serves queries, live tails,
tamper verification, and **signed audit reports** over **gRPC/TLS** — with
**Prometheus/Grafana** observability and **offline/online licensed** sessions.

## Quickstart

```powershell
# one-time setup: PKI, keys, licenses, config
go run ./tools/gencerts
go run ./cmd/lictl keygen -name issuer
go run ./cmd/lictl keygen -name signing
go run ./cmd/lictl issue -subject station-01 -features ingest
go run ./cmd/lictl issue -subject "*" -features query -out operator.lic
copy config\devlogd.example.yaml config\devlogd.yaml

# backing services (Redis, MinIO, Prometheus, Grafana)
docker compose -f deploy/docker-compose.yml up -d

# the service
go run ./cmd/devlogd -config config/devlogd.yaml

# in another terminal: simulate a sanitization job, then look at it
go run ./cmd/logctl sim    -device station-01 -license station-01.lic
go run ./cmd/logctl query  -license operator.lic -since 15m
go run ./cmd/logctl verify -license operator.lic -device station-01
go run ./cmd/logctl export -license operator.lic -trace <job-id-from-sim>
```

Grafana: <http://localhost:3000> (admin/admin) → **Device Logger** dashboard.

## Documentation

| Document | Contents |
| --- | --- |
| [docs/MANUAL.md](docs/MANUAL.md) | setup, configuration reference, licensing, C++ integration contract, operations runbook |
| [CONTRIBUTING.md](CONTRIBUTING.md) | code formatting/style conventions and how to extend the codebase (new component, new client, proto changes, new docs) |
| [docs/DESIGN.md](docs/DESIGN.md) | guiding principles, architecture, pattern inventory, security model, NIST/IEEE compliance mapping, failure modes, deployment & multi-language client rationale |
| [docs/COMPONENTS.md](docs/COMPONENTS.md) | technical reference for every component — responsibility, concepts, public surface, interactions, failure behaviour |
| [docs/components/](docs/components/README.md) | zero-to-hero Go deep-dive per component — an ordered learning path teaching the language through the real code |
| [docs/TESTING.md](docs/TESTING.md) | the verification mechanism — Go unit tests, the multi-language client harness, the C++ supervisor test, Postman/grpcurl integration |
| [docs/GO_CONCEPTS.md](docs/GO_CONCEPTS.md) | one-line-per-concept Go index, linking into `docs/components/` for the full treatment |
| [clients/README.md](clients/README.md) | Node, NestJS, and Python (Django/Flask/FastAPI) reference clients — both planes |
| [deploy/embed/README.md](deploy/embed/README.md) | embedding devlogd in a C++ app (spawn supervisor) + `.deb` packaging |
| [postman/README.md](postman/README.md) | Postman gRPC collection + grpcurl CI runner |
| [examples/cpp/publisher](examples/cpp/publisher) | reference C++ producer (paho-mqtt-cpp + protobuf) |

## Layout

```
api/proto/      protobuf contract (LogEntry, SanitizationEvent, LogService)
api/gen/        generated Go code (committed)
cmd/devlogd     the service          cmd/licensed   mini license server
cmd/lictl       license issuer CLI   cmd/logctl     query/tail/verify/export/sim CLI
internal/       broker, ingest, sign, license, hot, cold, archive, query, grpcapi, config, telemetry
deploy/         Dockerfile, docker-compose, Prometheus, Grafana provisioning + dashboard
tools/gencerts  dev PKI generator
```

## Development

```powershell
go build ./...   # build everything
go test ./...    # unit tests (no infrastructure needed)
go vet ./...     # static analysis
buf generate     # regenerate protobuf code after editing api/proto
```
