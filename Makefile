# Development targets. On Windows without make, run the commands directly
# (they are listed in docs/MANUAL.md).

GO ?= go

.PHONY: gen lint build vet test certs keys license operator-license run up down sim

gen:            ## regenerate protobuf code
	buf generate

lint:           ## proto lint
	buf lint

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

certs:          ## dev PKI: CA + server + client certificates
	$(GO) run ./tools/gencerts

keys:           ## license-issuer and entry-signing keypairs
	$(GO) run ./cmd/lictl keygen -name issuer
	$(GO) run ./cmd/lictl keygen -name signing

license:        ## device license for the simulator / C++ app
	$(GO) run ./cmd/lictl issue -subject station-01 -features ingest -max-sessions 2

operator-license: ## operator license for logctl / dashboards
	$(GO) run ./cmd/lictl issue -subject "*" -features query -out operator.lic

run:
	$(GO) run ./cmd/devlogd -config config/devlogd.yaml

up:             ## backing services: redis, minio, prometheus, grafana
	docker compose -f deploy/docker-compose.yml up -d

down:
	docker compose -f deploy/docker-compose.yml down

sim:
	$(GO) run ./cmd/logctl sim -device station-01 -license station-01.lic
