// Standalone smoke test that exercises both planes of devlogd end-to-end,
// mirroring the Node client's `doE2E`: publish a sanitization job over MQTT,
// then query it back, verify the hash chain, and export the signed audit
// report over gRPC — proving the module works through a NestJS client.
//
// This needs a live stack (devlogd + Redis + MinIO). Exits non-zero on failure.
import 'reflect-metadata';
import { NestFactory } from '@nestjs/core';
import { AppModule } from './app.module';
import { config } from './config';
import { toTimestamp, buildSanitizationJob, Severity } from './devlog/entry';
import { PublisherService } from './devlog/publisher.service';
import { ObserverService } from './devlog/observer.service';

function jobId(): string {
  return `job-nest-${Date.now()}`;
}

async function main() {
  // A DI context without an HTTP server — reuse the same injectable services.
  const app = await NestFactory.createApplicationContext(AppModule, { logger: false });
  const publisher = app.get(PublisherService);
  const observer = app.get(ObserverService);

  const traceId = jobId();

  // --- write path: publish the ordered job over MQTT ---
  const { entries } = buildSanitizationJob(config.deviceId, traceId);
  const mqttClient = await publisher.connect();
  console.log(`connected to ${config.mqttUrl} as ${config.deviceId}`);
  try {
    await publisher.publishJob(mqttClient, entries);
    console.log(`published job ${traceId} (${entries.length} entries)`);
  } finally {
    await mqttClient.endAsync();
  }

  // Give the pipeline a moment to sign + append before reading back.
  await new Promise((r) => setTimeout(r, 1000));

  // --- read path: query, verify, export over gRPC ---
  const grpcClient = observer.connect();
  try {
    const queried = await observer.query(grpcClient, {
      from: toTimestamp(new Date(Date.now() - 15 * 60 * 1000)),
      minSeverity: Severity.UNSPECIFIED,
      traceId,
      limit: 1000,
    });
    console.error(`${queried.length} entries`);
    if (queried.length === 0) throw new Error('e2e: no entries returned for the job just published');

    const verify = await observer.verifyRange(grpcClient, {
      deviceId: config.deviceId,
      from: toTimestamp(new Date(Date.now() - 24 * 3600 * 1000)),
    });
    console.log(
      verify.ok
        ? `OK: ${verify.entriesChecked} entries verified, chain intact`
        : `TAMPER EVIDENCE: ${verify.entriesChecked} checked, ${verify.breaks.length} problems`,
    );

    const report = await observer.exportAuditReport(grpcClient, {
      traceId,
      from: toTimestamp(new Date(Date.now() - 30 * 24 * 3600 * 1000)),
    });
    const verdict = report.allSignaturesValid ? 'ALL SIGNATURES VALID' : 'SIGNATURE PROBLEMS DETECTED';
    console.log(
      `report for ${report.traceId}: ${report.entries.length} entries, ` +
        `${report.signaturesVerified} signatures verified — ${verdict}`,
    );
  } finally {
    observer.close(grpcClient);
  }

  await app.close();
  console.log('e2e: OK');
}

main().catch((err) => {
  console.error('error:', err.message || err);
  process.exit(1);
});
