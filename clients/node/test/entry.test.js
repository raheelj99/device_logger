// Unit tests for the pure builders and the protobuf codec. No devlogd, Redis,
// or MinIO required — these exercise message construction and wire encoding,
// which is where a client is most likely to diverge from the contract.
'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');

const {
  Severity,
  SanitizationPhase,
  topicFor,
  toTimestamp,
  buildLogEntry,
  buildSanitizationJob,
} = require('../src/entry');
const { encodeLogEntry } = require('../src/proto');
const protobuf = require('protobufjs');
const path = require('path');

test('topicFor pins to the device namespace the broker ACL enforces', () => {
  assert.equal(topicFor('station-01', 'sanitizer'), 'devlog/v1/station-01/sanitizer');
});

test('toTimestamp splits epoch millis into seconds + nanos', () => {
  const ts = toTimestamp(new Date(1_700_000_123_456));
  assert.equal(ts.seconds, 1_700_000_123);
  assert.equal(ts.nanos, 456_000_000);
});

test('buildLogEntry requires device and subsystem', () => {
  assert.throws(() => buildLogEntry({ subsystem: 's' }), /deviceId is required/);
  assert.throws(() => buildLogEntry({ deviceId: 'd' }), /subsystem is required/);
});

test('buildLogEntry never sets server-owned fields', () => {
  const e = buildLogEntry({ deviceId: 'd', subsystem: 's', message: 'm' });
  assert.equal(e.entryId, undefined);
  assert.equal(e.ingestTime, undefined);
  assert.equal(e.audit, undefined);
});

test('buildSanitizationJob emits the canonical ordered phase sequence', () => {
  const { traceId, entries } = buildSanitizationJob('station-01', 'job-x');
  assert.equal(traceId, 'job-x');
  assert.equal(entries.length, 6);
  assert.deepEqual(
    entries.map((e) => e.sanitization.phase),
    [
      SanitizationPhase.STARTED,
      SanitizationPhase.PROGRESS,
      SanitizationPhase.PROGRESS,
      SanitizationPhase.PROGRESS,
      SanitizationPhase.VERIFYING,
      SanitizationPhase.COMPLETED,
    ],
  );
  // seq is monotonic per device — the server relies on producer ordering.
  assert.deepEqual(entries.map((e) => e.seq), [1, 2, 3, 4, 5, 6]);
  // Only the terminal COMPLETED entry carries a verification result.
  assert.ok(entries[5].sanitization.verification.passed);
  assert.equal(entries[0].sanitization.verification, undefined);
});

test('encodeLogEntry round-trips through the shared .proto', () => {
  const e = buildLogEntry({
    deviceId: 'station-01',
    seq: 7,
    severity: Severity.WARN,
    subsystem: 'sanitizer',
    message: 'hello',
    traceId: 'job-x',
    attributes: { k: 'v' },
  });
  const bytes = encodeLogEntry(e);
  assert.ok(bytes.length > 0);

  // Decode independently to confirm field numbers/types match the contract.
  const root = new protobuf.Root();
  root.resolvePath = (_o, t) =>
    t.startsWith('google/protobuf/')
      ? protobuf.util.path.resolve(_o, t)
      : path.join(__dirname, '..', '..', 'proto', t);
  root.loadSync('devicelog/v1/log.proto', { keepCase: false });
  const LogEntry = root.lookupType('devicelog.v1.LogEntry');
  const decoded = LogEntry.toObject(LogEntry.decode(bytes), { enums: Number, longs: Number });

  assert.equal(decoded.deviceId, 'station-01');
  assert.equal(decoded.seq, 7);
  assert.equal(decoded.severity, Severity.WARN);
  assert.equal(decoded.subsystem, 'sanitizer');
  assert.equal(decoded.traceId, 'job-x');
  assert.deepEqual(decoded.attributes, { k: 'v' });
});

test('encodeLogEntry rejects a malformed entry', () => {
  // severity must be an enum number, not a string.
  assert.throws(() => encodeLogEntry({ deviceId: 'd', subsystem: 's', severity: 'nope' }));
});
