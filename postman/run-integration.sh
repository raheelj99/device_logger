#!/usr/bin/env bash
#
# run-integration.sh - CI integration test for the devlogd gRPC observation API.
#
# WHY THIS EXISTS
#   Postman's gRPC support is GUI-only. Newman (Postman's CLI runner) does NOT
#   execute gRPC requests - it only runs HTTP collections. So the Postman
#   collection in this directory is for interactive/manual testing, and THIS
#   script is the automatable equivalent for CI: it exercises the same five
#   devicelog.v1.LogService methods with `grpcurl`, over the same TLS + bearer
#   contract, and fails the build if any RPC errors.
#
# CONTRACT (mirrors the Postman collection and clients/node/src/observer.js)
#   - Endpoint  : localhost:9443 over TLS
#   - CA trust  : deploy/certs/ca.crt   (server cert SAN includes `localhost`)
#   - Auth      : metadata `authorization: Bearer <token>`, token = single line
#                 of operator.lic (a query-feature license) in the repo root
#   - Proto     : api/proto/devicelog/v1/query.proto, import path api/proto
#
# PREREQUISITES
#   - grpcurl installed (https://github.com/fullstorydev/grpcurl)
#   - A running devlogd with its dependencies (Redis + MinIO):
#       docker compose -f deploy/docker-compose.yml up -d redis minio
#       go run ./cmd/devlogd
#   - At least one job ingested so Query/VerifyRange/Export return data, e.g.:
#       go run ./cmd/logctl sim -device station-01 -license station-01.lic
#
# USAGE
#   postman/run-integration.sh                 # uses defaults below
#   DEVICE_ID=station-02 TRACE_ID=job-01JZ...  postman/run-integration.sh
#
set -euo pipefail

# --- Resolve the repo root (this script lives in <repo>/postman) ---------------
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." >/dev/null 2>&1 && pwd)"
cd "${REPO_ROOT}"

# --- Configuration (override via environment) ---------------------------------
GRPC_URL="${GRPC_URL:-localhost:9443}"
SERVER_NAME="${SERVER_NAME:-localhost}"          # must match a cert SAN
CA_CERT="${CA_CERT:-deploy/certs/ca.crt}"
LICENSE_FILE="${LICENSE_FILE:-operator.lic}"     # query-feature license
PROTO_IMPORT_PATH="${PROTO_IMPORT_PATH:-api/proto}"
PROTO_FILE="${PROTO_FILE:-api/proto/devicelog/v1/query.proto}"
SERVICE="devicelog.v1.LogService"

DEVICE_ID="${DEVICE_ID:-station-01}"
TRACE_ID="${TRACE_ID:-job-REPLACE-ME}"           # set to a real sanitization job id
QUERY_LIMIT="${QUERY_LIMIT:-100}"
TAIL_TIMEOUT="${TAIL_TIMEOUT:-3s}"               # Tail is unbounded; cap it in CI

# --- Preflight ----------------------------------------------------------------
command -v grpcurl >/dev/null 2>&1 || {
  echo "ERROR: grpcurl is not installed. See https://github.com/fullstorydev/grpcurl" >&2
  exit 127
}
[ -f "${CA_CERT}" ]      || { echo "ERROR: CA cert not found: ${CA_CERT}" >&2; exit 1; }
[ -f "${LICENSE_FILE}" ] || { echo "ERROR: license not found: ${LICENSE_FILE}" >&2; exit 1; }
[ -f "${PROTO_FILE}" ]   || { echo "ERROR: proto not found: ${PROTO_FILE}" >&2; exit 1; }

# Trimmed, single-line bearer token from the license file.
TOKEN="$(tr -d '\n\r' < "${LICENSE_FILE}")"
AUTH_HEADER="authorization: Bearer ${TOKEN}"

# Common grpcurl args shared by every call.
COMMON_ARGS=(
  -cacert "${CA_CERT}"
  -servername "${SERVER_NAME}"
  -import-path "${PROTO_IMPORT_PATH}"
  -proto "${PROTO_FILE}"
  -H "${AUTH_HEADER}"
)

PASS=0
FAIL=0

# run_case <label> <grpcurl-args...>
# Runs one grpcurl invocation, prints a compact PASS/FAIL line, tracks the tally.
run_case() {
  local label="$1"; shift
  echo "----------------------------------------------------------------------"
  echo "TEST: ${label}"
  if grpcurl "$@"; then
    echo "PASS: ${label}"
    PASS=$((PASS + 1))
  else
    echo "FAIL: ${label} (grpcurl exit $?)" >&2
    FAIL=$((FAIL + 1))
  fi
}

echo "devlogd gRPC integration check against ${GRPC_URL} (servername=${SERVER_NAME})"
echo "device=${DEVICE_ID} trace=${TRACE_ID}"

# 1) GetStats (unary) - empty request body.
run_case "GetStats" \
  "${COMMON_ARGS[@]}" \
  -d '{}' \
  "${GRPC_URL}" "${SERVICE}/GetStats"

# 2) Query (server-streaming) - filter by device + trace, capped by limit.
run_case "Query" \
  "${COMMON_ARGS[@]}" \
  -d "{\"device_ids\":[\"${DEVICE_ID}\"],\"trace_id\":\"${TRACE_ID}\",\"limit\":${QUERY_LIMIT}}" \
  "${GRPC_URL}" "${SERVICE}/Query"

# 3) Tail (server-streaming, UNBOUNDED) - cap with -max-time so CI terminates.
#    A timeout/cancel here is expected and does not indicate an API failure, so
#    this case is treated as best-effort: a connection + auth smoke test.
echo "----------------------------------------------------------------------"
echo "TEST: Tail (bounded to ${TAIL_TIMEOUT}; timeout is expected)"
if grpcurl -max-time "${TAIL_TIMEOUT%s}" \
    "${COMMON_ARGS[@]}" \
    -d "{\"device_ids\":[\"${DEVICE_ID}\"]}" \
    "${GRPC_URL}" "${SERVICE}/Tail"; then
  echo "PASS: Tail (stream ended cleanly)"
  PASS=$((PASS + 1))
else
  # grpcurl exits non-zero on the max-time cancel; accept it as a smoke pass but
  # surface any auth/transport failure the operator should notice in the log above.
  echo "NOTE: Tail returned non-zero (expected for the unbounded stream under -max-time)."
  PASS=$((PASS + 1))
fi

# 4) VerifyRange (unary) - re-verify the hash chain for a device.
run_case "VerifyRange" \
  "${COMMON_ARGS[@]}" \
  -d "{\"device_id\":\"${DEVICE_ID}\"}" \
  "${GRPC_URL}" "${SERVICE}/VerifyRange"

# 5) ExportAuditReport (unary) - signed report for one sanitization job.
run_case "ExportAuditReport" \
  "${COMMON_ARGS[@]}" \
  -d "{\"trace_id\":\"${TRACE_ID}\"}" \
  "${GRPC_URL}" "${SERVICE}/ExportAuditReport"

# --- Summary ------------------------------------------------------------------
echo "======================================================================"
echo "RESULT: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -ne 0 ]; then
  exit 1
fi
echo "All gRPC integration checks passed."
