// Central configuration for the NestJS client. Everything is overridable by
// environment so the same code runs in dev, CI, and a real deployment.
//
// The connection contract (identical for every language client):
//   - MQTT ingest : mqtts://<host>:8883, TLS(ca.crt), username=device id,
//                   password=license token (an `ingest`-feature .lic).
//   - gRPC observe: <host>:9443, TLS(ca.crt), authorization: Bearer <token>
//                   (a `query`-feature .lic).

import * as path from 'path';

// Repository root, relative to clients/nest/src.
const REPO_ROOT = path.resolve(__dirname, '..', '..', '..');

function env(name: string, fallback: string): string {
  const v = process.env[name];
  return v === undefined || v === '' ? fallback : v;
}

export interface DevlogConfig {
  host: string;
  mqttPort: number;
  grpcPort: number;
  deviceId: string;
  ingestLicenseFile: string;
  queryLicenseFile: string;
  caFile: string;
  protoDir: string;
  logProto: string;
  queryProto: string;
  grpcTarget: string;
  mqttUrl: string;
}

export const config: DevlogConfig = (() => {
  // Hostname the certificate is valid for (a SAN in deploy/certs/server.crt).
  const host = env('DEVLOG_HOST', 'localhost');
  const mqttPort = Number(env('DEVLOG_MQTT_PORT', '8883'));
  const grpcPort = Number(env('DEVLOG_GRPC_PORT', '9443'));

  return {
    host,
    mqttPort,
    grpcPort,

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

    grpcTarget: `${host}:${grpcPort}`,
    mqttUrl: `mqtts://${host}:${mqttPort}`,
  };
})();

export default config;
