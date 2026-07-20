#!/usr/bin/env python3
# CLI that exercises both planes of devlogd end-to-end, mirroring `logctl` and
# the Node client's index.js:
#   publish | query | verify | export | stats | tail | e2e
#
# `e2e` is the full smoke test: publish a sanitization job over MQTT, then
# query it back, verify the hash chain, and export the signed audit report over
# gRPC — proving the module works through a non-Go client.
from __future__ import annotations

import sys
import time

from google.protobuf.json_format import MessageToJson

from devlog_client.config import config
from devlog_client.entry import Severity, build_sanitization_job, to_timestamp
from devlog_client.gen.devicelog.v1 import query_pb2
from devlog_client.observer import Observer
from devlog_client.publisher import Publisher


def _job_id() -> str:
    return f"job-python-{int(time.time() * 1000)}"


def do_publish(trace_id: str) -> None:
    _, entries = build_sanitization_job(config.device_id, trace_id)
    pub = Publisher().connect()
    print(f"connected to {config.mqtt_url} as {config.device_id}")
    try:
        pub.publish_job(entries)
        print(f"published job {trace_id} ({len(entries)} entries)")
    finally:
        pub.close()


def do_query(trace_id: str = "", since_ms: int = 15 * 60 * 1000, min_severity: int = Severity.SEVERITY_UNSPECIFIED):
    obs = Observer().connect()
    try:
        req = query_pb2.QueryRequest(min_severity=min_severity, limit=1000)
        getattr(req, "from").CopyFrom(to_timestamp(time.time() - since_ms / 1000))
        if trace_id:
            req.trace_id = trace_id
        entries = obs.query(req)
        for e in entries:
            print(MessageToJson(e, indent=None))
        print(f"{len(entries)} entries", file=sys.stderr)
        return entries
    finally:
        obs.close()


def do_verify(device: str = ""):
    device = device or config.device_id
    obs = Observer().connect()
    try:
        req = query_pb2.VerifyRangeRequest(device_id=device)
        getattr(req, "from").CopyFrom(to_timestamp(time.time() - 24 * 3600))
        resp = obs.verify_range(req)
        if resp.ok:
            print(f"OK: {resp.entries_checked} entries verified, chain intact")
        else:
            print(f"TAMPER EVIDENCE: {resp.entries_checked} checked, {len(resp.breaks)} problems")
            for b in resp.breaks:
                print(f"  {b.entry_id}: {b.reason}")
        return resp
    finally:
        obs.close()


def do_export(trace_id: str):
    if not trace_id:
        raise ValueError("export requires a trace id")
    obs = Observer().connect()
    try:
        req = query_pb2.ExportAuditReportRequest(trace_id=trace_id)
        getattr(req, "from").CopyFrom(to_timestamp(time.time() - 30 * 24 * 3600))
        rep = obs.export_audit_report(req)
        verdict = "ALL SIGNATURES VALID" if rep.all_signatures_valid else "SIGNATURE PROBLEMS DETECTED"
        print(
            f"report for {rep.trace_id}: {len(rep.entries)} entries, "
            f"{rep.signatures_verified} signatures verified — {verdict}"
        )
        return rep
    finally:
        obs.close()


def do_stats():
    obs = Observer().connect()
    try:
        resp = obs.get_stats()
        for d in resp.devices:
            last = "set" if d.HasField("last_ingest") else "never"
            print(f"{d.device_id}\thot_entries={d.hot_entries}\tlast_ingest={last}")
        return resp
    finally:
        obs.close()


def do_tail():
    obs = Observer().connect()
    print("tailing (Ctrl-C to stop)…", file=sys.stderr)
    try:
        for e in obs.tail(query_pb2.TailRequest()):
            print(MessageToJson(e, indent=None))
    except KeyboardInterrupt:
        pass
    finally:
        obs.close()


def do_e2e() -> None:
    """The headline test: write path then read path, all from Python."""
    trace_id = _job_id()
    do_publish(trace_id)
    # Give the pipeline a moment to sign + append before reading back.
    time.sleep(1.0)
    entries = do_query(trace_id=trace_id)
    if not entries:
        raise RuntimeError("e2e: no entries returned for the job just published")
    do_verify()
    do_export(trace_id)
    print("e2e: OK")


def main(argv=None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    cmd = argv[0] if argv else "e2e"
    arg = argv[1] if len(argv) > 1 else None
    try:
        if cmd == "publish":
            do_publish(arg or _job_id())
        elif cmd == "query":
            do_query(trace_id=arg or "")
        elif cmd == "verify":
            do_verify(arg or "")
        elif cmd == "export":
            do_export(arg or "")
        elif cmd == "stats":
            do_stats()
        elif cmd == "tail":
            do_tail()
        elif cmd == "e2e":
            do_e2e()
        else:
            print(f"unknown command {cmd}; use publish|query|verify|export|stats|tail|e2e", file=sys.stderr)
            return 2
    except Exception as err:  # noqa: BLE001 — CLI top level mirrors Node's catch
        print(f"error: {err}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
