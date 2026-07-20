# Pure builders for the devicelog.v1 wire types. These construct real protobuf
# LogEntry messages from the generated stubs but perform no I/O, so they are
# trivially unit-testable and their output serializes byte-for-byte the same as
# every other language client (one schema, no drift).
from __future__ import annotations

import time
from typing import Dict, List, Mapping, Optional, Tuple

from devlog_client.gen.devicelog.v1 import log_pb2

# Mirror of the devicelog.v1 enums by their proto numeric values. The generated
# module exposes the SEVERITY_* names; these aliases match the Node client's
# short spellings for readability and cross-client parity.
Severity = log_pb2.Severity
SanitizationPhase = log_pb2.SanitizationPhase
SanitizationStandard = log_pb2.SanitizationStandard

# Short aliases (numeric values are identical to the proto enum entries).
SEVERITY_INFO = Severity.SEVERITY_INFO
SEVERITY_WARN = Severity.SEVERITY_WARN


def topic_for(device_id: str, subsystem: str) -> str:
    """The MQTT topic a device publishes to. The broker's ACL pins a session to
    devlog/v1/<its-own-id>/#, so device must equal the authenticated identity."""
    return f"devlog/v1/{device_id}/{subsystem}"


def to_timestamp(seconds_float: Optional[float] = None):
    """google.protobuf.Timestamp from epoch seconds (or now). Split into whole
    seconds + nanos exactly like the Node client's millis math."""
    if seconds_float is None:
        seconds_float = time.time()
    ms = int(round(seconds_float * 1000))
    from google.protobuf.timestamp_pb2 import Timestamp

    ts = Timestamp()
    ts.seconds = ms // 1000
    ts.nanos = (ms % 1000) * 1_000_000
    return ts


def build_log_entry(
    *,
    device_id: str,
    subsystem: str,
    seq: int = 0,
    severity: int = None,
    message: str = "",
    trace_id: str = "",
    device_time: Optional[float] = None,
    attributes: Optional[Mapping[str, str]] = None,
    payload: Optional[bytes] = None,
    sanitization: Optional[log_pb2.SanitizationEvent] = None,
) -> log_pb2.LogEntry:
    """Build a LogEntry protobuf message. entry_id, ingest_time and audit are
    deliberately never set — the server owns them and rejects producer-supplied
    values."""
    if not device_id:
        raise ValueError("deviceId is required")
    if not subsystem:
        raise ValueError("subsystem is required")
    if severity is None:
        severity = Severity.SEVERITY_INFO

    e = log_pb2.LogEntry(
        device_id=device_id,
        seq=seq,
        severity=severity,
        subsystem=subsystem,
        message=message,
        trace_id=trace_id,
    )
    e.device_time.CopyFrom(to_timestamp(device_time))
    if attributes:
        for k, v in attributes.items():
            e.attributes[k] = v
    if payload:
        e.payload = payload
    if sanitization is not None:
        e.sanitization.CopyFrom(sanitization)
    return e


def build_sanitization_job(device_id: str, trace_id: str) -> Tuple[str, List[log_pb2.LogEntry]]:
    """A complete, ordered sanitization job — the same sequence the C++ and Node
    reference producers emit: STARTED -> 3x PROGRESS -> VERIFYING -> COMPLETED.
    Returns (trace_id, entries) so callers can publish then query it back."""
    media = log_pb2.TargetMedia(
        serial="WD-9F2K3L0042",
        model="WDC WD40EFRX",
        capacity_bytes=4 * 2 ** 40,  # 4 TiB
        media_type="HDD",
    )

    seq = 0

    def step(phase: int, message: str, *, progress_pct: float = None, verification=None) -> log_pb2.LogEntry:
        nonlocal seq
        seq += 1
        san = log_pb2.SanitizationEvent(
            media=media,
            standard=SanitizationStandard.SANITIZATION_STANDARD_NIST_800_88_PURGE,
            technique="overwrite-1pass",
            operator_id="op-python",
            phase=phase,
        )
        if progress_pct is not None:
            san.progress_pct = progress_pct
        if verification is not None:
            san.verification.CopyFrom(verification)
        return build_log_entry(
            device_id=device_id,
            seq=seq,
            severity=Severity.SEVERITY_INFO,
            subsystem="sanitizer",
            message=message,
            trace_id=trace_id,
            sanitization=san,
        )

    P = SanitizationPhase
    entries = [
        step(P.SANITIZATION_PHASE_STARTED, "sanitization started"),
        step(P.SANITIZATION_PHASE_PROGRESS, "overwrite pass in progress", progress_pct=25),
        step(P.SANITIZATION_PHASE_PROGRESS, "overwrite pass in progress", progress_pct=50),
        step(P.SANITIZATION_PHASE_PROGRESS, "overwrite pass in progress", progress_pct=75),
        step(P.SANITIZATION_PHASE_VERIFYING, "verifying sampled sectors", progress_pct=100),
        step(
            P.SANITIZATION_PHASE_COMPLETED,
            "sanitization completed, verification passed",
            progress_pct=100,
            verification=log_pb2.Verification(method="sampled-read", sample_pct=10, passed=True),
        ),
    ]
    return trace_id, entries
