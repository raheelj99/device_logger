// Ingest plane: publish LogEntry protobuf over MQTT/TLS, authenticating with
// the device license as the MQTT password (username = device id).

import { Injectable } from '@nestjs/common';
import * as fs from 'fs';
import * as mqtt from 'mqtt';
import { config } from '../config';
import { encodeLogEntry } from './proto';
import { topicFor, LogEntry } from './entry';

@Injectable()
export class PublisherService {
  // Connect to the broker with TLS + license credentials. Resolves once the
  // CONNACK is accepted (i.e. the license passed the broker auth hook).
  connect(): Promise<mqtt.MqttClient> {
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
  publishEntry(client: mqtt.MqttClient, entry: LogEntry): Promise<void> {
    const payload = Buffer.from(encodeLogEntry(entry));
    const topic = topicFor(entry.deviceId, entry.subsystem);
    return new Promise((resolve, reject) => {
      client.publish(topic, payload, { qos: 1 }, (err) => (err ? reject(err) : resolve()));
    });
  }

  // Publish an ordered job (from buildSanitizationJob) sequentially so the
  // per-device hash chain is built in the intended order.
  async publishJob(client: mqtt.MqttClient, entries: LogEntry[]): Promise<void> {
    for (const e of entries) {
      await this.publishEntry(client, e);
      process.stdout.write(`-> [${e.seq}] ${e.message}\n`);
    }
  }
}
