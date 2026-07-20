// Observation plane: the gRPC LogService client. TLS + a per-RPC bearer token
// (the query license). RequireTransportSecurity is implicit — the call
// credentials only attach over the TLS channel credentials below.
'use strict';

const fs = require('fs');
const grpc = require('@grpc/grpc-js');
const config = require('./config');
const { loadService } = require('./proto');

// Build the bearer metadata generator from a token. Exposed for unit testing.
function bearerCallCreds(token) {
  return grpc.credentials.createFromMetadataGenerator((_params, cb) => {
    const md = new grpc.Metadata();
    md.set('authorization', `Bearer ${token}`);
    cb(null, md);
  });
}

// Dial devlogd with channel TLS + call-time bearer credentials combined.
function connect() {
  const LogService = loadService();
  const token = fs.readFileSync(config.queryLicenseFile, 'utf8').trim();
  const ssl = grpc.credentials.createSsl(fs.readFileSync(config.caFile));
  const combined = grpc.credentials.combineChannelCredentials(ssl, bearerCallCreds(token));
  return new LogService(config.grpcTarget, combined, {
    // Match the certificate SAN when dialing by IP or an alias.
    'grpc.ssl_target_name_override': config.host,
    'grpc.default_authority': config.host,
  });
}

// Unary helper -> Promise.
function unary(client, method, req) {
  return new Promise((resolve, reject) => {
    client[method](req, (err, resp) => (err ? reject(err) : resolve(resp)));
  });
}

// Server-streaming helper: collect the whole stream into an array.
function collectStream(client, method, req) {
  return new Promise((resolve, reject) => {
    const out = [];
    const call = client[method](req);
    call.on('data', (e) => out.push(e));
    call.on('error', reject);
    call.on('end', () => resolve(out));
  });
}

const query = (client, req) => collectStream(client, 'Query', req);
const verifyRange = (client, req) => unary(client, 'VerifyRange', req);
const exportAuditReport = (client, req) => unary(client, 'ExportAuditReport', req);
const getStats = (client) => unary(client, 'GetStats', {});

// Tail is unbounded; the caller controls lifetime and receives entries via cb.
// Returns the call so the caller can cancel().
function tail(client, req, onEntry) {
  const call = client.Tail(req);
  call.on('data', onEntry);
  return call;
}

function close(client) {
  grpc.closeClient(client);
}

module.exports = {
  bearerCallCreds,
  connect,
  query,
  verifyRange,
  exportAuditReport,
  getStats,
  tail,
  close,
};
