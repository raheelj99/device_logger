// Unit tests for the pure builders and the protobuf codec. No devlogd, Redis,
// or MinIO required — these exercise message construction and wire encoding,
// which is where a client is most likely to diverge from the contract.

import * as path from 'path';
import * as protobuf from 'protobufjs';
import {
  Severity,
  SanitizationPhase,
  topicFor,
  toTimestamp,
  buildLogEntry,
  buildSanitizationJob,
} from './entry';
import { encodeLogEntry } from './proto';

describe('devicelog.v1 builders + codec', () => {
  test('topicFor pins to the device namespace the broker ACL enforces', () => {
    expect(topicFor('station-01', 'sanitizer')).toBe('devlog/v1/station-01/sanitizer');
  });

  test('toTimestamp splits epoch millis into seconds + nanos', () => {
    const ts = toTimestamp(new Date(1_700_000_123_456));
    expect(ts.seconds).toBe(1_700_000_123);
    expect(ts.nanos).toBe(456_000_000);
  });

  test('buildLogEntry requires device and subsystem', () => {
    expect(() => buildLogEntry({ subsystem: 's' } as any)).toThrow(/deviceId is required/);
    expect(() => buildLogEntry({ deviceId: 'd' } as any)).toThrow(/subsystem is required/);
  });

  test('buildLogEntry never sets server-owned fields', () => {
    const e = buildLogEntry({ deviceId: 'd', subsystem: 's', message: 'm' }) as any;
    expect(e.entryId).toBeUndefined();
    expect(e.ingestTime).toBeUndefined();
    expect(e.audit).toBeUndefined();
  });

  test('buildSanitizationJob emits the canonical ordered phase sequence', () => {
    const { traceId, entries } = buildSanitizationJob('station-01', 'job-x');
    expect(traceId).toBe('job-x');
    expect(entries.length).toBe(6);
    expect(entries.map((e) => (e.sanitization as any).phase)).toEqual([
      SanitizationPhase.STARTED,
      SanitizationPhase.PROGRESS,
      SanitizationPhase.PROGRESS,
      SanitizationPhase.PROGRESS,
      SanitizationPhase.VERIFYING,
      SanitizationPhase.COMPLETED,
    ]);
    // seq is monotonic per device — the server relies on producer ordering.
    expect(entries.map((e) => e.seq)).toEqual([1, 2, 3, 4, 5, 6]);
    // Only the terminal COMPLETED entry carries a verification result.
    expect((entries[5].sanitization as any).verification.passed).toBe(true);
    expect((entries[0].sanitization as any).verification).toBeUndefined();
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
    expect(bytes.length).toBeGreaterThan(0);

    // Decode independently to confirm field numbers/types match the contract.
    const root = new protobuf.Root();
    root.resolvePath = (o, t) =>
      t.startsWith('google/protobuf/')
        ? protobuf.util.path.resolve(o, t)
        : path.join(__dirname, '..', '..', '..', 'proto', t);
    root.loadSync('devicelog/v1/log.proto', { keepCase: false });
    const LogEntry = root.lookupType('devicelog.v1.LogEntry');
    const decoded = LogEntry.toObject(LogEntry.decode(bytes), { enums: Number, longs: Number }) as any;

    expect(decoded.deviceId).toBe('station-01');
    expect(decoded.seq).toBe(7);
    expect(decoded.severity).toBe(Severity.WARN);
    expect(decoded.subsystem).toBe('sanitizer');
    expect(decoded.traceId).toBe('job-x');
    expect(decoded.attributes).toEqual({ k: 'v' });
  });

  test('encodeLogEntry rejects a malformed entry', () => {
    // severity must be an enum number, not a string.
    expect(() => encodeLogEntry({ deviceId: 'd', subsystem: 's', severity: 'nope' })).toThrow();
  });
});
