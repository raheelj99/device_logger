#!/usr/bin/env node
// CLI that exercises both planes of devlogd end-to-end, mirroring `logctl`:
//   publish | query | verify | export | stats | tail | e2e
//
// `e2e` is the full smoke test: publish a sanitization job over MQTT, then
// query it back, verify the hash chain, and export the signed audit report
// over gRPC — proving the module works through a non-Go client.
'use strict';

const config = require('./config');
const { toTimestamp, buildSanitizationJob, Severity } = require('./entry');
const publisher = require('./publisher');
const observer = require('./observer');

function jobId() {
  return `job-node-${Date.now()}`;
}

async function doPublish(traceId) {
  const { entries } = buildSanitizationJob(config.deviceId, traceId);
  const client = await publisher.connect();
  console.log(`connected to ${config.mqttUrl} as ${config.deviceId}`);
  try {
    await publisher.publishJob(client, entries);
    console.log(`published job ${traceId} (${entries.length} entries)`);
  } finally {
    await client.endAsync();
  }
}

async function doQuery({ traceId, sinceMs = 15 * 60 * 1000, minSeverity = Severity.UNSPECIFIED } = {}) {
  const client = observer.connect();
  try {
    const req = {
      from: toTimestamp(new Date(Date.now() - sinceMs)),
      minSeverity,
      limit: 1000,
    };
    if (traceId) req.traceId = traceId;
    const entries = await observer.query(client, req);
    for (const e of entries) console.log(JSON.stringify(e));
    console.error(`${entries.length} entries`);
    return entries;
  } finally {
    observer.close(client);
  }
}

async function doVerify(device = config.deviceId) {
  const client = observer.connect();
  try {
    const resp = await observer.verifyRange(client, {
      deviceId: device,
      from: toTimestamp(new Date(Date.now() - 24 * 3600 * 1000)),
    });
    if (resp.ok) {
      console.log(`OK: ${resp.entriesChecked} entries verified, chain intact`);
    } else {
      console.log(`TAMPER EVIDENCE: ${resp.entriesChecked} checked, ${resp.breaks.length} problems`);
      for (const b of resp.breaks) console.log(`  ${b.entryId}: ${b.reason}`);
    }
    return resp;
  } finally {
    observer.close(client);
  }
}

async function doExport(traceId) {
  if (!traceId) throw new Error('export requires a trace id');
  const client = observer.connect();
  try {
    const rep = await observer.exportAuditReport(client, {
      traceId,
      from: toTimestamp(new Date(Date.now() - 30 * 24 * 3600 * 1000)),
    });
    const verdict = rep.allSignaturesValid ? 'ALL SIGNATURES VALID' : 'SIGNATURE PROBLEMS DETECTED';
    console.log(
      `report for ${rep.traceId}: ${rep.entries.length} entries, ` +
        `${rep.signaturesVerified} signatures verified — ${verdict}`,
    );
    return rep;
  } finally {
    observer.close(client);
  }
}

async function doStats() {
  const client = observer.connect();
  try {
    const resp = await observer.getStats(client);
    for (const d of resp.devices || []) {
      console.log(`${d.deviceId}\thot_entries=${d.hotEntries}\tlast_ingest=${d.lastIngest ? 'set' : 'never'}`);
    }
    return resp;
  } finally {
    observer.close(client);
  }
}

async function doTail() {
  const client = observer.connect();
  console.error('tailing (Ctrl-C to stop)…');
  const call = observer.tail(client, {}, (e) => console.log(JSON.stringify(e)));
  process.on('SIGINT', () => {
    call.cancel();
    observer.close(client);
    process.exit(0);
  });
}

// The headline test: write path then read path, all from Node.
async function doE2E() {
  const traceId = jobId();
  await doPublish(traceId);
  // Give the pipeline a moment to sign + append before reading back.
  await new Promise((r) => setTimeout(r, 1000));
  const entries = await doQuery({ traceId });
  if (entries.length === 0) throw new Error('e2e: no entries returned for the job just published');
  await doVerify();
  await doExport(traceId);
  console.log('e2e: OK');
}

async function main() {
  const cmd = process.argv[2] || 'e2e';
  const arg = process.argv[3];
  switch (cmd) {
    case 'publish': return doPublish(arg || jobId());
    case 'query':   return void (await doQuery({ traceId: arg }));
    case 'verify':  return void (await doVerify(arg));
    case 'export':  return void (await doExport(arg));
    case 'stats':   return doStats();
    case 'tail':    return doTail();
    case 'e2e':     return doE2E();
    default:
      console.error(`unknown command ${cmd}; use publish|query|verify|export|stats|tail|e2e`);
      process.exit(2);
  }
}

if (require.main === module) {
  main().catch((err) => {
    console.error('error:', err.message || err);
    process.exit(1);
  });
}

module.exports = { doPublish, doQuery, doVerify, doExport, doStats, doE2E };
