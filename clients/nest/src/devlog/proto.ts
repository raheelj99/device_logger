// Protobuf/gRPC loading. protobufjs handles raw LogEntry encoding for the MQTT
// payload; @grpc/proto-loader builds the LogService client stub. Both read the
// exact .proto files the Go service is generated from — one schema, no drift.

import * as path from 'path';
import * as protobuf from 'protobufjs';
import * as protoLoader from '@grpc/proto-loader';
import * as grpc from '@grpc/grpc-js';
import { config } from '../config';

// --- MQTT payload codec (protobufjs) ---

let logEntryType: protobuf.Type | null = null;

// Lazily load and cache the LogEntry message type.
function logEntry(): protobuf.Type {
  if (logEntryType) return logEntryType;
  const root = new protobuf.Root();
  // Resolve imports (including query.proto -> log.proto) against clients/proto.
  root.resolvePath = (origin, target) => {
    if (target.startsWith('google/protobuf/')) return protobuf.util.path.resolve(origin, target);
    return path.join(config.protoDir, target);
  };
  root.loadSync(config.logProto, { keepCase: false });
  logEntryType = root.lookupType('devicelog.v1.LogEntry');
  return logEntryType;
}

// Encode a plain LogEntry object (from entry.ts) to protobuf bytes for MQTT.
export function encodeLogEntry(obj: unknown): Uint8Array {
  const type = logEntry();
  const err = type.verify(obj as { [k: string]: unknown });
  if (err) throw new Error(`invalid LogEntry: ${err}`);
  return type.encode(type.fromObject(obj as { [k: string]: unknown })).finish();
}

// --- gRPC service stub (@grpc/proto-loader) ---

export function loadLogService(): grpc.ServiceClientConstructor {
  const def = protoLoader.loadSync(config.queryProto, {
    keepCase: false,
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
    includeDirs: [config.protoDir],
  });
  const pkg = grpc.loadPackageDefinition(def) as any;
  return pkg.devicelog.v1.LogService as grpc.ServiceClientConstructor;
}
