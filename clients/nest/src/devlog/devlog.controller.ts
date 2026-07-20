// REST facade over devlogd's two planes. Each endpoint opens a short-lived
// client, does its work, and tears it down — a test/integration surface, not a
// pooled production gateway.

import { Controller, Get, Post, Param, Query, Body } from '@nestjs/common';
import { config } from '../config';
import { toTimestamp, buildSanitizationJob, Severity } from './entry';
import { PublisherService } from './publisher.service';
import { ObserverService } from './observer.service';

@Controller()
export class DevlogController {
  constructor(
    private readonly publisher: PublisherService,
    private readonly observer: ObserverService,
  ) {}

  // Publish a full sanitization job over MQTT and return its trace id.
  @Post('jobs')
  async publishJob(@Body() body: { traceId?: string; deviceId?: string } = {}) {
    const deviceId = body.deviceId || config.deviceId;
    const traceId = body.traceId || `job-nest-${Date.now()}`;
    const { entries } = buildSanitizationJob(deviceId, traceId);
    const client = await this.publisher.connect();
    try {
      await this.publisher.publishJob(client, entries);
    } finally {
      await client.endAsync();
    }
    return { traceId, deviceId, published: entries.length };
  }

  // Query historical entries, optionally scoped by trace id and lookback.
  @Get('entries')
  async entries(@Query('traceId') traceId?: string, @Query('since') since?: string) {
    const sinceMs = since ? Number(since) : 15 * 60 * 1000;
    const client = this.observer.connect();
    try {
      const req: Record<string, unknown> = {
        from: toTimestamp(new Date(Date.now() - sinceMs)),
        minSeverity: Severity.UNSPECIFIED,
        limit: 1000,
      };
      if (traceId) req.traceId = traceId;
      const entries = await this.observer.query(client, req);
      return { count: entries.length, entries };
    } finally {
      this.observer.close(client);
    }
  }

  // Verify the per-device hash chain over the last 24h.
  @Get('verify/:deviceId')
  async verify(@Param('deviceId') deviceId: string) {
    const client = this.observer.connect();
    try {
      return await this.observer.verifyRange(client, {
        deviceId,
        from: toTimestamp(new Date(Date.now() - 24 * 3600 * 1000)),
      });
    } finally {
      this.observer.close(client);
    }
  }

  // Export the signed audit report for one sanitization job.
  @Get('report/:traceId')
  async report(@Param('traceId') traceId: string) {
    const client = this.observer.connect();
    try {
      return await this.observer.exportAuditReport(client, {
        traceId,
        from: toTimestamp(new Date(Date.now() - 30 * 24 * 3600 * 1000)),
      });
    } finally {
      this.observer.close(client);
    }
  }

  // Per-device hot-tier counts.
  @Get('stats')
  async stats() {
    const client = this.observer.connect();
    try {
      return await this.observer.getStats(client);
    } finally {
      this.observer.close(client);
    }
  }
}
