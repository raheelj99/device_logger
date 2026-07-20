// Ingest plane: publish LogEntry protobuf over MQTT/TLS, authenticating with
// the device license as the MQTT password (username = device id).
'use strict';

const fs = require('fs');
const mqtt = require('mqtt');
const config = require('./config');
const { encodeLogEntry } = require('./proto');
const { topicFor } = require('./entry');

// Connect to the broker with TLS + license credentials. Resolves once the
// CONNACK is accepted (i.e. the license passed the broker auth hook).
function connect() {
  const token = fs.readFileSync(config.ingestLicenseFile, 'utf8').trim();
  const client = mqtt.connect(config.mqttUrl, {
    username: config.deviceId,
    password: token,
    ca: [fs.readFileSync(config.caFile)],
    // The cert is issued for the SAN, not necessarily the dialed host.
    rejectUnauthorized: true,
    servername: config.host,
    protocolVersion: 4,
    reconnectPeriod: 0, // fail fast rather than silently retry in a test client
    connectTimeout: 10_000,
  });

  return new Promise((resolve, reject) => {
    client.once('connect', () => resolve(client));
    client.once('error', (err) => {
      client.end(true);
      reject(err);
    });
  });
}

// Publish one entry at QoS 1 (at-least-once): the resolve fires after PUBACK,
// mirroring the C++ producer's `->wait()`.
function publishEntry(client, entry) {
  const payload = Buffer.from(encodeLogEntry(entry));
  const topic = topicFor(entry.deviceId, entry.subsystem);
  return new Promise((resolve, reject) => {
    client.publish(topic, payload, { qos: 1 }, (err) => (err ? reject(err) : resolve()));
  });
}

// Publish an ordered job (from buildSanitizationJob) sequentially so the
// per-device hash chain is built in the intended order.
async function publishJob(client, entries) {
  for (const e of entries) {
    await publishEntry(client, e);
    process.stdout.write(`-> [${e.seq}] ${e.message}\n`);
  }
}

module.exports = { connect, publishEntry, publishJob };
