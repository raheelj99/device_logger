# Skip @pytest.mark.integration tests by default: they need a live devlogd +
# Redis + MinIO. Opt in with `--integration` or by selecting `-m integration`.
import pytest


def pytest_addoption(parser):
    parser.addoption(
        "--integration",
        action="store_true",
        default=False,
        help="run integration tests that require a live devlogd deployment",
    )


def pytest_collection_modifyitems(config, items):
    # If the user explicitly selected the marker (-m integration), honor it.
    if config.getoption("--integration") or "integration" in (config.getoption("-m") or ""):
        return
    skip = pytest.mark.skip(reason="needs live devlogd (+ Redis + MinIO); use --integration")
    for item in items:
        if "integration" in item.keywords:
            item.add_marker(skip)
