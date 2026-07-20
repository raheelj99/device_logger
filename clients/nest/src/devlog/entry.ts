// Pure, dependency-free builders for the devicelog.v1 wire types. Kept free of
// I/O and protobuf runtime so they are trivially unit-testable; proto.ts turns
// the plain objects these produce into encoded bytes.

// Mirror of the devicelog.v1 enums (proto numeric values).
export const Severity = {
  UNSPECIFIED: 0, TRACE: 1, DEBUG: 2, INFO: 3, WARN: 4, ERROR: 5, FATAL: 6,
} as const;

export const SanitizationPhase = {
  UNSPECIFIED: 0, STARTED: 1, PROGRESS: 2, VERIFYING: 3,
  COMPLETED: 4, FAILED: 5, ABORTED: 6,
} as const;

export const SanitizationStandard = {
  UNSPECIFIED: 0,
  NIST_800_88_CLEAR: 1, NIST_800_88_PURGE: 2, NIST_800_88_DESTROY: 3,
  IEEE_2883_CLEAR: 4, IEEE_2883_PURGE: 5, IEEE_2883_DESTRUCT: 6,
} as const;

export interface Timestamp {
  seconds: number;
  nanos: number;
}

// google.protobuf.Timestamp from a JS Date (or now).
export function toTimestamp(date: Date = new Date()): Timestamp {
  const ms = date.getTime();
  return { seconds: Math.floor(ms / 1000), nanos: (ms % 1000) * 1e6 };
}

// The MQTT topic a device publishes to. The broker's ACL pins a session to
// devlog/v1/<its-own-id>/#, so device must equal the authenticated identity.
export function topicFor(deviceId: string, subsystem: string): string {
  return `devlog/v1/${deviceId}/${subsystem}`;
}

export interface LogEntry {
  deviceId: string;
  seq?: number;
  severity: number;
  subsystem: string;
  message?: string;
  traceId?: string;
  deviceTime: Timestamp;
  attributes?: Record<string, string>;
  payload?: Uint8Array;
  sanitization?: Record<string, unknown>;
}

export interface BuildLogEntryArgs {
  deviceId: string;
  seq?: number;
  severity?: number;
  subsystem: string;
  message?: string;
  traceId?: string;
  deviceTime?: Date;
  attributes?: Record<string, string>;
  payload?: Uint8Array;
  sanitization?: Record<string, unknown>;
}

// Build a LogEntry as a plain object using protobufjs camelCase field names.
// entry_id, ingest_time and audit are deliberately omitted — the server owns
// them and rejects producer-supplied values.
export function buildLogEntry({
  deviceId,
  seq,
  severity = Severity.INFO,
  subsystem,
  message,
  traceId,
  deviceTime = new Date(),
  attributes,
  payload,
  sanitization,
}: BuildLogEntryArgs): LogEntry {
  if (!deviceId) throw new Error('deviceId is required');
  if (!subsystem) throw new Error('subsystem is required');
  const e: LogEntry = {
    deviceId,
    seq,
    severity,
    subsystem,
    message,
    traceId,
    deviceTime: toTimestamp(deviceTime),
  };
  if (attributes) e.attributes = attributes;
  if (payload) e.payload = payload;
  if (sanitization) e.sanitization = sanitization;
  return e;
}

export interface SanitizationJob {
  traceId: string;
  entries: LogEntry[];
}

// A complete, ordered sanitization job — the same sequence the C++ reference
// producer and `logctl sim` emit: STARTED -> 3x PROGRESS -> VERIFYING ->
// COMPLETED. Returns { traceId, entries } so callers can publish then query it.
export function buildSanitizationJob(deviceId: string, traceId: string): SanitizationJob {
  const media = {
    serial: 'WD-9F2K3L0042',
    model: 'WDC WD40EFRX',
    capacityBytes: 4 * 2 ** 40,
    mediaType: 'HDD',
  };
  const base = {
    media,
    standard: SanitizationStandard.NIST_800_88_PURGE,
    technique: 'overwrite-1pass',
    operatorId: 'op-nest',
  };
  let seq = 0;
  const step = (phase: number, message: string, extra: Record<string, unknown> = {}) =>
    buildLogEntry({
      deviceId,
      seq: ++seq,
      severity: Severity.INFO,
      subsystem: 'sanitizer',
      message,
      traceId,
      sanitization: { ...base, phase, ...extra },
    });

  const entries = [
    step(SanitizationPhase.STARTED, 'sanitization started'),
    step(SanitizationPhase.PROGRESS, 'overwrite pass in progress', { progressPct: 25 }),
    step(SanitizationPhase.PROGRESS, 'overwrite pass in progress', { progressPct: 50 }),
    step(SanitizationPhase.PROGRESS, 'overwrite pass in progress', { progressPct: 75 }),
    step(SanitizationPhase.VERIFYING, 'verifying sampled sectors', { progressPct: 100 }),
    step(SanitizationPhase.COMPLETED, 'sanitization completed, verification passed', {
      progressPct: 100,
      verification: { method: 'sampled-read', samplePct: 10, passed: true },
    }),
  ];
  return { traceId, entries };
}
