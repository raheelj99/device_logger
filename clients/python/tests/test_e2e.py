# Full end-to-end test against a LIVE devlogd (+ Redis + MinIO). Skipped by
# default — it needs the whole deployment running, which the unit suite does
# not. Enable with:  pytest -m integration
import time

import pytest

from devlog_client import service


@pytest.mark.integration
def test_publish_query_verify_export_round_trip():
    trace_id = f"job-pytest-{int(time.time() * 1000)}"

    published = service.publish_job(trace_id=trace_id)
    assert published["published"] == 6
    assert published["trace_id"] == trace_id

    # Give the pipeline a moment to sign + append before reading back.
    time.sleep(1.0)

    entries = service.query_entries(trace_id=trace_id)
    assert len(entries) > 0, "no entries returned for the job just published"

    verdict = service.verify_range()
    assert verdict.get("ok") is True

    report = service.export_report(trace_id)
    assert report.get("trace_id") == trace_id
    assert report.get("all_signatures_valid") is True
