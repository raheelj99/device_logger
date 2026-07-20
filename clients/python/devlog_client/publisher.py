# Ingest plane: publish LogEntry protobuf over MQTT/TLS, authenticating with
# the device license as the MQTT password (username = device id).
from __future__ import annotations

import ssl
import threading
from typing import Iterable, Optional

import paho.mqtt.client as mqtt

from devlog_client.config import Config, config as default_config
from devlog_client.entry import topic_for
from devlog_client.gen.devicelog.v1 import log_pb2


class Publisher:
    """MQTT publisher for the devlogd ingest plane. Connect once, then publish
    entries or whole jobs at QoS 1 (at-least-once), matching the C++/Node
    producers' `->wait()` semantics."""

    def __init__(self, cfg: Optional[Config] = None):
        self.cfg = cfg or default_config
        self._client: Optional[mqtt.Client] = None
        self._connected = threading.Event()
        self._connect_rc: Optional[int] = None

    def connect(self, timeout: float = 10.0) -> "Publisher":
        """Connect to the broker with TLS + license credentials. Blocks until the
        CONNACK is accepted (i.e. the license passed the broker auth hook)."""
        token = self.cfg.read_ingest_license()
        client = mqtt.Client(
            mqtt.CallbackAPIVersion.VERSION2,
            protocol=mqtt.MQTTv311,  # protocolVersion: 4
        )
        client.username_pw_set(self.cfg.device_id, token)

        # TLS trusting the deployment CA; the cert is issued for the SAN, not
        # necessarily the dialed host, so pin the servername to config.host.
        ctx = ssl.create_default_context(cafile=self.cfg.ca_file)
        client.tls_set_context(ctx)
        client.tls_insecure_set(False)
        client.host = self.cfg.host  # used as the TLS servername for SNI/verify

        def on_connect(_c, _u, _flags, reason_code, _props):
            self._connect_rc = int(reason_code.value) if hasattr(reason_code, "value") else int(reason_code)
            self._connected.set()

        client.on_connect = on_connect
        client.connect(self.cfg.host, self.cfg.mqtt_port)
        client.loop_start()

        if not self._connected.wait(timeout):
            client.loop_stop()
            raise TimeoutError(f"MQTT connect to {self.cfg.mqtt_url} timed out")
        if self._connect_rc != 0:
            client.loop_stop()
            raise ConnectionError(f"MQTT connect refused (reason code {self._connect_rc})")

        self._client = client
        return self

    def publish_entry(self, entry: log_pb2.LogEntry) -> None:
        """Publish one entry at QoS 1; blocks until PUBACK."""
        if self._client is None:
            raise RuntimeError("not connected")
        payload = entry.SerializeToString()
        topic = topic_for(entry.device_id, entry.subsystem)
        info = self._client.publish(topic, payload, qos=1)
        info.wait_for_publish()

    def publish_job(self, entries: Iterable[log_pb2.LogEntry]) -> None:
        """Publish an ordered job (from build_sanitization_job) sequentially so
        the per-device hash chain is built in the intended order."""
        for e in entries:
            self.publish_entry(e)
            print(f"-> [{e.seq}] {e.message}")

    def close(self) -> None:
        if self._client is not None:
            self._client.loop_stop()
            self._client.disconnect()
            self._client = None

    def __enter__(self) -> "Publisher":
        return self.connect()

    def __exit__(self, *_exc) -> None:
        self.close()
