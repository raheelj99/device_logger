# A thin, framework-agnostic service layer over Publisher/Observer that returns
# plain JSON-serializable dicts. The Django/Flask/FastAPI apps are all trivial
# wrappers around these functions — one implementation, no per-framework drift.
from __future__ import annotations

import time
from typing import Any, Dict, List

from google.protobuf.json_format import MessageToDict

from devlog_client.config import config
from devlog_client.entry import Severity, build_sanitization_job, to_timestamp
from devlog_client.gen.devicelog.v1 import query_pb2
from devlog_client.observer import Observer
from devlog_client.publisher import Publisher


def _to_dict(msg) -> Dict[str, Any]:
    return MessageToDict(msg, preserving_proto_field_name=True)


def publish_job(trace_id: str = "", device_id: str = "") -> Dict[str, Any]:
    """Publish a canonical sanitization job over MQTT. Generates a trace id when
    the caller doesn't supply one."""
    device_id = device_id or config.device_id
    trace_id = trace_id or f"job-python-{int(time.time() * 1000)}"
    _, entries = build_sanitization_job(device_id, trace_id)
    pub = Publisher().connect()
    try:
        pub.publish_job(entries)
    finally:
        pub.close()
    return {"trace_id": trace_id, "device_id": device_id, "published": len(entries)}


def query_entries(trace_id: str = "", since_ms: int = 15 * 60 * 1000,
                  min_severity: int = Severity.SEVERITY_UNSPECIFIED, limit: int = 1000) -> List[Dict[str, Any]]:
    obs = Observer().connect()
    try:
        req = query_pb2.QueryRequest(min_severity=min_severity, limit=limit)
        getattr(req, "from").CopyFrom(to_timestamp(time.time() - since_ms / 1000))
        if trace_id:
            req.trace_id = trace_id
        return [_to_dict(e) for e in obs.query(req)]
    finally:
        obs.close()


def verify_range(device_id: str = "", since_ms: int = 24 * 3600 * 1000) -> Dict[str, Any]:
    device_id = device_id or config.device_id
    obs = Observer().connect()
    try:
        req = query_pb2.VerifyRangeRequest(device_id=device_id)
        getattr(req, "from").CopyFrom(to_timestamp(time.time() - since_ms / 1000))
        return _to_dict(obs.verify_range(req))
    finally:
        obs.close()


def export_report(trace_id: str, since_ms: int = 30 * 24 * 3600 * 1000) -> Dict[str, Any]:
    if not trace_id:
        raise ValueError("export requires a trace id")
    obs = Observer().connect()
    try:
        req = query_pb2.ExportAuditReportRequest(trace_id=trace_id)
        getattr(req, "from").CopyFrom(to_timestamp(time.time() - since_ms / 1000))
        return _to_dict(obs.export_audit_report(req))
    finally:
        obs.close()


def get_stats() -> Dict[str, Any]:
    obs = Observer().connect()
    try:
        return _to_dict(obs.get_stats())
    finally:
        obs.close()
