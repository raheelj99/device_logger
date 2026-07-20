# Observation plane: the gRPC LogService client. TLS + a per-RPC bearer token
# (the query license). The call credentials only attach over the TLS channel
# credentials below, so the token is never sent in the clear.
from __future__ import annotations

from typing import Callable, Iterator, List, Optional

import grpc

from devlog_client.config import Config, config as default_config
from devlog_client.gen.devicelog.v1 import log_pb2, query_pb2, query_pb2_grpc


class _BearerAuth(grpc.AuthMetadataPlugin):
    """Attaches `authorization: Bearer <token>` to every call. Runs only over
    the secure channel, mirroring the Node client's call credentials."""

    def __init__(self, token: str):
        self._token = token

    def __call__(self, _context, callback):
        callback((("authorization", f"Bearer {self._token}"),), None)


class Observer:
    """gRPC client for the devlogd observation plane."""

    def __init__(self, cfg: Optional[Config] = None):
        self.cfg = cfg or default_config
        self._channel: Optional[grpc.Channel] = None
        self._stub: Optional[query_pb2_grpc.LogServiceStub] = None

    def connect(self) -> "Observer":
        """Dial devlogd with channel TLS + call-time bearer credentials combined."""
        token = self.cfg.read_query_license()
        with open(self.cfg.ca_file, "rb") as fh:
            ca = fh.read()
        ssl_creds = grpc.ssl_channel_credentials(root_certificates=ca)
        call_creds = grpc.metadata_call_credentials(_BearerAuth(token))
        combined = grpc.composite_channel_credentials(ssl_creds, call_creds)
        # Match the certificate SAN when dialing by IP or an alias.
        options = (
            ("grpc.ssl_target_name_override", self.cfg.host),
            ("grpc.default_authority", self.cfg.host),
        )
        self._channel = grpc.secure_channel(self.cfg.grpc_target, combined, options)
        self._stub = query_pb2_grpc.LogServiceStub(self._channel)
        return self

    def _require(self) -> query_pb2_grpc.LogServiceStub:
        if self._stub is None:
            raise RuntimeError("not connected")
        return self._stub

    def query(self, req: query_pb2.QueryRequest) -> List[log_pb2.LogEntry]:
        """Server-streaming Query, collected into a list (matches Node)."""
        return list(self._require().Query(req))

    def verify_range(self, req: query_pb2.VerifyRangeRequest) -> query_pb2.VerifyRangeResponse:
        return self._require().VerifyRange(req)

    def export_audit_report(self, req: query_pb2.ExportAuditReportRequest) -> query_pb2.AuditReport:
        return self._require().ExportAuditReport(req)

    def get_stats(self) -> query_pb2.GetStatsResponse:
        return self._require().GetStats(query_pb2.GetStatsRequest())

    def tail(self, req: query_pb2.TailRequest, on_entry: Optional[Callable[[log_pb2.LogEntry], None]] = None) -> Iterator[log_pb2.LogEntry]:
        """Tail is unbounded; the caller controls lifetime. Yields entries, and
        optionally invokes on_entry for each (cancel by breaking the loop)."""
        stream = self._require().Tail(req)
        for e in stream:
            if on_entry is not None:
                on_entry(e)
            yield e

    def close(self) -> None:
        if self._channel is not None:
            self._channel.close()
            self._channel = None
            self._stub = None

    def __enter__(self) -> "Observer":
        return self.connect()

    def __exit__(self, *_exc) -> None:
        self.close()
