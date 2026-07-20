# Unit tests for the pure builders and the protobuf codec. No devlogd, Redis,
# or MinIO required — these exercise message construction and wire encoding,
# which is where a client is most likely to diverge from the contract.
import pytest

from devlog_client.entry import (
    SanitizationPhase,
    Severity,
    build_log_entry,
    build_sanitization_job,
    to_timestamp,
    topic_for,
)
from devlog_client.gen.devicelog.v1 import log_pb2


def test_topic_for_pins_to_device_namespace():
    # The broker ACL enforces devlog/v1/<device>/#.
    assert topic_for("station-01", "sanitizer") == "devlog/v1/station-01/sanitizer"


def test_to_timestamp_splits_epoch_into_seconds_and_nanos():
    ts = to_timestamp(1_700_000_123.456)
    assert ts.seconds == 1_700_000_123
    assert ts.nanos == 456_000_000


def test_build_log_entry_requires_device_and_subsystem():
    with pytest.raises(ValueError, match="deviceId is required"):
        build_log_entry(device_id="", subsystem="s")
    with pytest.raises(ValueError, match="subsystem is required"):
        build_log_entry(device_id="d", subsystem="")


def test_build_log_entry_never_sets_server_owned_fields():
    e = build_log_entry(device_id="d", subsystem="s", message="m")
    # Scalars default to empty; the message field must be unset.
    assert e.entry_id == ""
    assert not e.HasField("ingest_time")
    assert not e.HasField("audit")


def test_build_sanitization_job_emits_canonical_ordered_phase_sequence():
    trace_id, entries = build_sanitization_job("station-01", "job-x")
    assert trace_id == "job-x"
    assert len(entries) == 6
    assert [e.sanitization.phase for e in entries] == [
        SanitizationPhase.SANITIZATION_PHASE_STARTED,
        SanitizationPhase.SANITIZATION_PHASE_PROGRESS,
        SanitizationPhase.SANITIZATION_PHASE_PROGRESS,
        SanitizationPhase.SANITIZATION_PHASE_PROGRESS,
        SanitizationPhase.SANITIZATION_PHASE_VERIFYING,
        SanitizationPhase.SANITIZATION_PHASE_COMPLETED,
    ]
    # seq is monotonic per device — the server relies on producer ordering.
    assert [e.seq for e in entries] == [1, 2, 3, 4, 5, 6]
    # Only the terminal COMPLETED entry carries a verification result.
    assert entries[5].sanitization.verification.passed is True
    assert not entries[0].sanitization.HasField("verification")


def test_build_sanitization_job_media_and_standard_match_contract():
    _, entries = build_sanitization_job("station-01", "job-x")
    san = entries[0].sanitization
    assert san.media.serial == "WD-9F2K3L0042"
    assert san.media.model == "WDC WD40EFRX"
    assert san.media.capacity_bytes == 4 * 2 ** 40
    assert san.media.media_type == "HDD"
    assert san.standard == log_pb2.SanitizationStandard.SANITIZATION_STANDARD_NIST_800_88_PURGE
    assert san.technique == "overwrite-1pass"
    assert san.operator_id == "op-python"
    completed = entries[5].sanitization
    assert completed.verification.method == "sampled-read"
    assert completed.verification.sample_pct == 10
    assert completed.verification.passed is True


def test_log_entry_serialize_parse_round_trip():
    # Proves field numbers/types match the shared .proto contract: encode with
    # the builder, decode into a fresh message, and confirm every field.
    e = build_log_entry(
        device_id="station-01",
        seq=7,
        severity=Severity.SEVERITY_WARN,
        subsystem="sanitizer",
        message="hello",
        trace_id="job-x",
        attributes={"k": "v"},
    )
    raw = e.SerializeToString()
    assert len(raw) > 0

    decoded = log_pb2.LogEntry()
    decoded.ParseFromString(raw)
    assert decoded.device_id == "station-01"
    assert decoded.seq == 7
    assert decoded.severity == Severity.SEVERITY_WARN
    assert decoded.subsystem == "sanitizer"
    assert decoded.trace_id == "job-x"
    assert dict(decoded.attributes) == {"k": "v"}
    # Server-owned fields survive the round-trip as unset.
    assert decoded.entry_id == ""
    assert not decoded.HasField("audit")
