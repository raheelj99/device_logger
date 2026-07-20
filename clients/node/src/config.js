// Central configuration for the Node client. Everything is overridable by
// environment so the same code runs in dev, CI, and a real deployment.
//
// The connection contract (identical for every language client):
//   - MQTT ingest : mqtts://<host>:8883, TLS(ca.crt), username=device id,
//                   password=license token (an `ingest`-feature .lic).
//   - gRPC observe: <host>:9443, TLS(ca.crt), authorization: Bearer <token>
//                   (a `query`-feature .lic).
'use strict';

const path = require('path');

// Repository root, relative to clients/node/src.
const REPO_ROOT = path.resolve(__dirname, '..', '..', '..');

function env(name, fallback) {
  const v = process.env[name];
  return v === undefined || v === '' ? fallback : v;
}

const config = {
  // Hostname the certificate is valid for (a SAN in deploy/certs/server.crt).
  host: env('DEVLOG_HOST', 'localhost'),
  mqttPort: Number(env('DEVLOG_MQTT_PORT', '8883')),
  grpcPort: Number(env('DEVLOG_GRPC_PORT', '9443')),

  // Identity + credentials.
  deviceId: env('DEVLOG_DEVICE_ID', 'station-01'),
  ingestLicenseFile: env('DEVLOG_INGEST_LICENSE', path.join(REPO_ROOT, 'station-01.lic')),
  queryLicenseFile: env('DEVLOG_QUERY_LICENSE', path.join(REPO_ROOT, 'operator.lic')),

  // TLS trust anchor (the deployment CA).
  caFile: env('DEVLOG_CA_FILE', path.join(REPO_ROOT, 'deploy', 'certs', 'ca.crt')),

  // Protobuf contract — the single source of truth shared with the Go service.
  protoDir: env('DEVLOG_PROTO_DIR', path.resolve(__dirname, '..', '..', 'proto')),
  logProto: 'devicelog/v1/log.proto',
  queryProto: 'devicelog/v1/query.proto',
};

config.grpcTarget = `${config.host}:${config.grpcPort}`;
config.mqttUrl = `mqtts://${config.host}:${config.mqttPort}`;

module.exports = config;
