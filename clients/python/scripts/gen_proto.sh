#!/usr/bin/env bash
# Generate the devicelog.v1 Python stubs from the shared .proto contract.
#
# Source of truth is clients/proto (the same files the Go service and the Node
# client build from — one schema, no drift). We root --proto_path there so the
# `import "devicelog/v1/log.proto"` in query.proto resolves, and emit into
# devlog_client/gen as an importable package.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PY_ROOT="$(cd "${HERE}/.." && pwd)"
PROTO_ROOT="$(cd "${PY_ROOT}/../proto" && pwd)"
OUT="${PY_ROOT}/devlog_client/gen"

# Prefer the venv interpreter when present so grpc_tools is available.
PYTHON="${PYTHON:-python3}"
if [[ -x "${PY_ROOT}/.venv/bin/python" ]]; then
  PYTHON="${PY_ROOT}/.venv/bin/python"
fi

mkdir -p "${OUT}"

"${PYTHON}" -m grpc_tools.protoc \
  --proto_path="${PROTO_ROOT}" \
  --python_out="${OUT}" \
  --grpc_python_out="${OUT}" \
  "devicelog/v1/log.proto" \
  "devicelog/v1/query.proto"

# protoc lays the output out as devicelog/v1/*.py mirroring the package path.
# Make every generated directory an importable package.
find "${OUT}" -type d -exec touch {}/__init__.py \;

# The generated modules import each other with the fully-qualified proto path
# (`from devicelog.v1 import log_pb2 ...`). Re-root those imports at the gen
# package so they resolve as devlog_client.gen.devicelog.v1.* regardless of the
# process's sys.path.
GEN_PKG="devlog_client.gen"
find "${OUT}/devicelog" -name '*_pb2*.py' -print0 | while IFS= read -r -d '' f; do
  # `from devicelog.v1 import log_pb2` -> `from devlog_client.gen.devicelog.v1 import log_pb2`
  sed -i -E "s/^from devicelog\.v1 import /from ${GEN_PKG}.devicelog.v1 import /" "${f}"
done

echo "generated stubs under ${OUT}/devicelog/v1:"
ls -1 "${OUT}/devicelog/v1"
