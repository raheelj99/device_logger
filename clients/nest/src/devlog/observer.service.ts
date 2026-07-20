// Observation plane: the gRPC LogService client. TLS + a per-RPC bearer token
// (the query license). RequireTransportSecurity is implicit — the call
// credentials only attach over the TLS channel credentials below.

import { Injectable } from '@nestjs/common';
import * as fs from 'fs';
import * as grpc from '@grpc/grpc-js';
import { config } from '../config';
import { loadLogService } from './proto';

@Injectable()
export class ObserverService {
  // Build the bearer metadata generator from a token. Exposed for unit testing.
  bearerCallCreds(token: string): grpc.CallCredentials {
    return grpc.credentials.createFromMetadataGenerator((_params, cb) => {
      const md = new grpc.Metadata();
      md.set('authorization', `Bearer ${token}`);
      cb(null, md);
    });
  }

  // Dial devlogd with channel TLS + call-time bearer credentials combined.
  connect(): grpc.Client {
    const LogService = loadLogService();
    const token = fs.readFileSync(config.queryLicenseFile, 'utf8').trim();
    const ssl = grpc.credentials.createSsl(fs.readFileSync(config.caFile));
    const combined = grpc.credentials.combineChannelCredentials(ssl, this.bearerCallCreds(token));
    return new LogService(config.grpcTarget, combined, {
      // Match the certificate SAN when dialing by IP or an alias.
      'grpc.ssl_target_name_override': config.host,
      'grpc.default_authority': config.host,
    });
  }

  // Unary helper -> Promise.
  private unary<T>(client: any, method: string, req: unknown): Promise<T> {
    return new Promise((resolve, reject) => {
      client[method](req, (err: Error | null, resp: T) => (err ? reject(err) : resolve(resp)));
    });
  }

  // Server-streaming helper: collect the whole stream into an array.
  private collectStream<T>(client: any, method: string, req: unknown): Promise<T[]> {
    return new Promise((resolve, reject) => {
      const out: T[] = [];
      const call = client[method](req);
      call.on('data', (e: T) => out.push(e));
      call.on('error', reject);
      call.on('end', () => resolve(out));
    });
  }

  query(client: grpc.Client, req: unknown): Promise<any[]> {
    return this.collectStream(client, 'Query', req);
  }

  verifyRange(client: grpc.Client, req: unknown): Promise<any> {
    return this.unary(client, 'VerifyRange', req);
  }

  exportAuditReport(client: grpc.Client, req: unknown): Promise<any> {
    return this.unary(client, 'ExportAuditReport', req);
  }

  getStats(client: grpc.Client): Promise<any> {
    return this.unary(client, 'GetStats', {});
  }

  // Tail is unbounded; the caller controls lifetime and receives entries via cb.
  // Returns the call so the caller can cancel().
  tail(client: grpc.Client, req: unknown, onEntry: (e: any) => void): grpc.ClientReadableStream<any> {
    const call = (client as any).Tail(req);
    call.on('data', onEntry);
    return call;
  }

  close(client: grpc.Client): void {
    grpc.closeClient(client);
  }
}
