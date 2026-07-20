// Protobuf/gRPC loading. protobufjs handles raw LogEntry encoding for the MQTT
// payload; @grpc/proto-loader builds the LogService client stub. Both read the
// exact .proto files the Go service is generated from — one schema, no drift.
'use strict';

const path = require('path');
const protobuf = require('protobufjs');
const protoLoader = require('@grpc/proto-loader');
const grpc = require('@grpc/grpc-js');
const config = require('./config');

// --- MQTT payload codec (protobufjs) ---

let logEntryType = null;

// Lazily load and cache the LogEntry message type.
function logEntry() {
  if (logEntryType) return logEntryType;
  const root = new protobuf.Root();
  // Resolve imports (including query.proto -> log.proto) against clients/proto.
  root.resolvePath = (_origin, target) => {
    if (target.startsWith('google/protobuf/')) return protobuf.util.path.resolve(_origin, target);
    return path.join(config.protoDir, target);
  };
  root.loadSync(config.logProto, { keepCase: false });
  logEntryType = root.lookupType('devicelog.v1.LogEntry');
  return logEntryType;
}

// Encode a plain LogEntry object (from entry.js) to protobuf bytes for MQTT.
function encodeLogEntry(obj) {
  const type = logEntry();
  const err = type.verify(obj);
  if (err) throw new Error(`invalid LogEntry: ${err}`);
  return type.encode(type.fromObject(obj)).finish();
}

// --- gRPC service stub (@grpc/proto-loader) ---

function loadService() {
  const def = protoLoader.loadSync(config.queryProto, {
    keepCase: false,
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
    includeDirs: [config.protoDir],
  });
  const pkg = grpc.loadPackageDefinition(def);
  return pkg.devicelog.v1.LogService;
}

module.exports = { encodeLogEntry, loadService };
