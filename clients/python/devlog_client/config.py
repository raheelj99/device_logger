# Central configuration for the Python client. Everything is overridable by
# environment so the same code runs in dev, CI, and a real deployment.
#
# The connection contract (identical for every language client):
#   - MQTT ingest : mqtts://<host>:8883, TLS(ca.crt), username=device id,
#                   password=license token (an `ingest`-feature .lic).
#   - gRPC observe: <host>:9443, TLS(ca.crt), authorization: Bearer <token>
#                   (a `query`-feature .lic).
import os
from dataclasses import dataclass, field
from pathlib import Path

# Repository root, relative to clients/python/devlog_client.
REPO_ROOT = Path(__file__).resolve().parents[3]


def _env(name: str, fallback: str) -> str:
    v = os.environ.get(name)
    return fallback if v is None or v == "" else v


@dataclass
class Config:
    # Hostname the certificate is valid for (a SAN in deploy/certs/server.crt).
    host: str = field(default_factory=lambda: _env("DEVLOG_HOST", "localhost"))
    mqtt_port: int = field(default_factory=lambda: int(_env("DEVLOG_MQTT_PORT", "8883")))
    grpc_port: int = field(default_factory=lambda: int(_env("DEVLOG_GRPC_PORT", "9443")))

    # Identity + credentials.
    device_id: str = field(default_factory=lambda: _env("DEVLOG_DEVICE_ID", "station-01"))
    ingest_license_file: str = field(
        default_factory=lambda: _env("DEVLOG_INGEST_LICENSE", str(REPO_ROOT / "station-01.lic"))
    )
    query_license_file: str = field(
        default_factory=lambda: _env("DEVLOG_QUERY_LICENSE", str(REPO_ROOT / "operator.lic"))
    )

    # TLS trust anchor (the deployment CA).
    ca_file: str = field(
        default_factory=lambda: _env("DEVLOG_CA_FILE", str(REPO_ROOT / "deploy" / "certs" / "ca.crt"))
    )

    @property
    def grpc_target(self) -> str:
        return f"{self.host}:{self.grpc_port}"

    @property
    def mqtt_url(self) -> str:
        return f"mqtts://{self.host}:{self.mqtt_port}"

    def read_ingest_license(self) -> str:
        return Path(self.ingest_license_file).read_text(encoding="utf-8").strip()

    def read_query_license(self) -> str:
        return Path(self.query_license_file).read_text(encoding="utf-8").strip()


# Module-level singleton mirroring the Node client's `config` export.
config = Config()
