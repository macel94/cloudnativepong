#!/usr/bin/env node
/**
 * Bounded local/disposable Pong journey smoke and load harness.
 *
 * Non-local targets require an explicit approved experiment marker. The
 * harness deliberately reports aggregate operation results only. Room IDs, names,
 * URLs, client addresses, tokens, and response bodies never appear in output.
 */
import process from 'node:process';
import { isIP } from 'node:net';
import WebSocket from 'ws';

const MAX_ITERATIONS = 50;
const MAX_CONCURRENCY = 8;
const MAX_TIMEOUT_MS = 30_000;
const MAX_DURATION_MS = 180_000;
const DEFAULT_ITERATIONS = 3;
const DEFAULT_CONCURRENCY = 1;
const DEFAULT_TIMEOUT_MS = 10_000;
const DEFAULT_DURATION_MS = 60_000;
const CANONICAL_PUBLIC_HOSTNAME = 'pong.belacca.com';
const EXPERIMENT_APPROVAL_VALUE = '1';

const flags = parseFlags(process.argv.slice(2));
const dryRun = flags.has('dry-run');
const configuredBase = flags.get('base-url') || process.env.LOAD_SMOKE_BASE_URL;
const baseURL = configuredBase || 'http://localhost:8080';
let iterations;
let concurrency;
let timeoutMs;
let maxDurationMs;
function configuredNumber(flagName, environmentName) {
  if (flags.has(flagName)) return flags.get(flagName);
  return process.env[environmentName] === '' ? undefined : process.env[environmentName];
}

try {
  iterations = boundedNumber(configuredNumber('iterations', 'LOAD_SMOKE_ITERATIONS'), DEFAULT_ITERATIONS, 1, MAX_ITERATIONS, 'iterations');
  concurrency = boundedNumber(configuredNumber('concurrency', 'LOAD_SMOKE_CONCURRENCY'), DEFAULT_CONCURRENCY, 1, MAX_CONCURRENCY, 'concurrency');
  timeoutMs = boundedNumber(configuredNumber('timeout-ms', 'LOAD_SMOKE_TIMEOUT_MS'), DEFAULT_TIMEOUT_MS, 500, MAX_TIMEOUT_MS, 'timeout-ms');
  maxDurationMs = boundedNumber(configuredNumber('max-duration-ms', 'LOAD_SMOKE_MAX_DURATION_MS'), DEFAULT_DURATION_MS, 1_000, MAX_DURATION_MS, 'max-duration-ms');
} catch (error) {
  console.error(`load smoke configuration rejected: ${error.message}`);
  process.exit(2);
}
const origin = process.env.LOAD_SMOKE_ORIGIN || inferOrigin(baseURL);

if (flags.has('help')) {
  console.log('usage: LOAD_SMOKE_BASE_URL=http://localhost:8080 node scripts/load-smoke.mjs [--dry-run]');
  console.log('non-local targets require LOAD_SMOKE_EXPERIMENT_APPROVED=1');
  process.exit(0);
}

let base;
try {
  base = new URL(baseURL);
  if (!['http:', 'https:'].includes(base.protocol)) throw new Error('unsupported protocol');
} catch {
  console.error('load smoke configuration rejected: base URL must use http or https');
  process.exit(2);
}

if (!isLocalTarget(base) && process.env.LOAD_SMOKE_EXPERIMENT_APPROVED !== EXPERIMENT_APPROVAL_VALUE) {
  const targetDescription = normalizeHostname(base.hostname) === CANONICAL_PUBLIC_HOSTNAME
    ? 'canonical public Pong production'
    : 'a non-local target';
  console.error(`load smoke configuration rejected: ${targetDescription} requires LOAD_SMOKE_EXPERIMENT_APPROVED=1`);
  process.exit(2);
}

if (dryRun) {
  console.log(JSON.stringify({
    mode: 'dry-run',
    operations: ['health', 'create', 'join', 'websocket', 'api_read', 'cleanup'],
    limits: { iterations, concurrency, timeout_ms: timeoutMs, max_duration_ms: maxDurationMs },
    output: 'aggregate counts and latency percentiles only',
  }, null, 2));
  process.exit(0);
}

if (!configuredBase) {
  console.error('load smoke requires LOAD_SMOKE_BASE_URL, or use --dry-run');
  process.exit(2);
}

const deadline = Date.now() + maxDurationMs;
const metrics = new Map(['health', 'create', 'join', 'websocket', 'api_read', 'cleanup'].map((name) => [name, { ok: 0, failed: 0, samples: [], statuses: new Map() }]));
const failures = new Map();
const httpStatuses = new Map();
const websocketStatuses = new Map();
let requestsTotal = 0;
let retryAfterResponses = 0;
let websocketRetryAfterResponses = 0;
let cleanupPollRequests = 0;
let completed = 0;
let nextIteration = 0;

function record(name, started, ok, failureCode = '', status = null) {
  const item = metrics.get(name);
  if (!item) return;
  if (status !== null) item.statuses.set(String(status), (item.statuses.get(String(status)) || 0) + 1);
  if (ok) {
    item.ok++;
    item.samples.push(Math.max(0, Date.now() - started));
  } else {
    item.failed++;
    if (failureCode) failures.set(failureCode, (failures.get(failureCode) || 0) + 1);
  }
}

function fail(code) {
  failures.set(code, (failures.get(code) || 0) + 1);
}

async function request(path, options = {}) {
  const started = Date.now();
  requestsTotal++;
  const remainingUntilDeadline = deadline - started;
  if (remainingUntilDeadline <= 0) return { response: null, body: '', started, failureCode: 'deadline' };
  const remainingMs = Math.min(timeoutMs, remainingUntilDeadline);

  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), remainingMs);
  try {
    const response = await fetch(endpoint(path), { ...options, signal: controller.signal });
    httpStatuses.set(String(response.status), (httpStatuses.get(String(response.status)) || 0) + 1);
    if (response.headers.has('retry-after')) retryAfterResponses++;
    const body = await response.text();
    return { response, body, started };
  } catch {
    return { response: null, body: '', started, failureCode: Date.now() >= deadline ? 'deadline' : 'transport' };
  } finally {
    clearTimeout(timer);
  }
}

async function jsonRequest(name, path, options = {}) {
  const result = await request(path, options);
  if (!result.response) {
    record(name, result.started, false, `${name}_${result.failureCode || 'transport'}`);
    return null;
  }
  const status = result.response.status;
  let body;
  try {
    body = JSON.parse(result.body);
  } catch {
    record(name, result.started, false, `${name}_body`, status);
    return null;
  }
  if (!result.response.ok) {
    record(name, result.started, false, `${name}_http_${statusClass(status)}`, status);
    return null;
  }
  record(name, result.started, true, '', status);
  return body;
}

async function runOne(sequence) {
  if (Date.now() >= deadline) {
    fail('deadline');
    return false;
  }

  const healthResult = await request('/health');
  if (!healthResult.response || !healthResult.response.ok) {
    record('health', healthResult.started, false, `health_${healthResult.failureCode || 'unavailable'}`, healthResult.response?.status ?? null);
    return false;
  }
  if (!healthResult.body.trim()) {
    record('health', healthResult.started, false, 'health_body', healthResult.response.status);
    return false;
  }
  record('health', healthResult.started, true, '', healthResult.response.status);

  const room = await jsonRequest('create', '/api/rooms/create', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ name: `load-smoke-${process.pid}-${sequence}` }),
  });
  if (!room || typeof room.id !== 'string' || !/^[0-9a-f]{6}$/.test(room.id)) {
    fail('create_contract');
    return false;
  }

  let sockets = [];
  // A created room owns the creator reservation. Always make one bounded
  // cleanup connection attempt when no player socket exists, including after a
  // rejected join, so overload does not leave a room waiting for reconciliation.
  let connectionAttempted = true;
  let cleanupStarted = Date.now();
  try {
    const joined = await jsonRequest('join', '/api/rooms/join', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ room_id: room.id }),
    });
    const validConnectionContract = joined && joined.room_id === room.id && (
      joined.ws_path === `/rooms/${room.id}/ws` || joined.mode === 'local'
    );
    if (!validConnectionContract) {
      fail('join_contract');
      return false;
    }

    connectionAttempted = true;
    const wsStarted = Date.now();
    const results = await Promise.allSettled([
      openPlayer(room.id),
      openPlayer(room.id),
    ]);
    sockets = results.filter((result) => result.status === 'fulfilled').map((result) => result.value);
    for (const result of results) {
      if (result.status === 'rejected' && result.reason?.status) {
        const status = String(result.reason.status);
        websocketStatuses.set(status, (websocketStatuses.get(status) || 0) + 1);
        if (result.reason.retryAfter) websocketRetryAfterResponses++;
      }
    }
    if (results.some((result) => result.status === 'rejected')) {
      record('websocket', wsStarted, false, 'websocket_contract');
      return false;
    }
    record('websocket', wsStarted, true);
    return true;
  } finally {
    cleanupStarted = Date.now();
    for (const socket of sockets) {
      try { socket.close(); } catch { /* best effort */ }
    }
    // If the journey failed before a socket was established, make one bounded
    // best-effort connection so the room lifecycle can still signal finished.
    if (connectionAttempted && sockets.length === 0) {
      try {
        const socket = await openPlayer(room.id);
        socket.close();
      } catch {
        // Reconciliation remains the final safety net for abandoned rooms.
      }
    }
    const cleanup = await waitForCleanup(room.id, cleanupStarted);
    record('cleanup', cleanup.started, cleanup.ok, cleanup.ok ? '' : 'cleanup_timeout');
  }
}

function openPlayer(roomID) {
  return new Promise((resolve, reject) => {
    const remainingUntilDeadline = deadline - Date.now();
    if (remainingUntilDeadline <= 0) {
      reject(new Error('deadline'));
      return;
    }
    const remainingMs = Math.min(timeoutMs, remainingUntilDeadline);
    const socket = new WebSocket(websocketEndpoint(`/rooms/${roomID}/ws`), {
      headers: { Origin: origin },
      handshakeTimeout: remainingMs,
    });
    let joined = false;
    let state = false;
    let settled = false;
    const timer = setTimeout(() => finish(new Error(Date.now() >= deadline ? 'deadline' : 'timeout')), remainingMs);
    const finish = (error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      if (error) {
        try { socket.close(); } catch { /* best effort */ }
        reject(error);
      } else {
        resolve(socket);
      }
    };
    socket.on('open', () => {
      try { socket.send(JSON.stringify({ type: 'proxy-ready' })); } catch { finish(new Error('send')); }
    });
    socket.on('message', (payload) => {
      try {
        const message = JSON.parse(payload.toString());
        if (message.type === 'error') return finish(new Error('server'));
        joined ||= message.type === 'joined';
        state ||= message.type === 'state';
        if (joined && state) finish();
      } catch {
        finish(new Error('message'));
      }
    });
    socket.on('unexpected-response', (_request, response) => {
      finish(Object.assign(new Error('socket'), {
        status: response.statusCode,
        retryAfter: response.headers['retry-after'] !== undefined,
      }));
    });
    socket.on('error', () => finish(new Error('socket')));
    socket.on('close', () => {
      if (!settled) finish(new Error('closed'));
    });
  });
}

async function waitForCleanup(roomID, started) {
  const cleanupDeadline = Math.min(deadline, Date.now() + timeoutMs);
  while (Date.now() < cleanupDeadline) {
    cleanupPollRequests++;
    const result = await request('/api/rooms');
    if (result.response) {
      try {
        const rooms = JSON.parse(result.body);
        const validRead = result.response.ok && Array.isArray(rooms);
        record('api_read', result.started, validRead, validRead ? '' : `api_read_${result.response.ok ? 'body' : `http_${statusClass(result.response.status)}`}`, result.response.status);
        if (validRead && !rooms.some((room) => room && room.id === roomID)) {
          return { ok: true, started };
        }
      } catch {
        record('api_read', result.started, false, 'api_read_body', result.response.status);
        // Retry until the bounded deadline.
      }
    } else {
      record('api_read', result.started, false, `api_read_${result.failureCode || 'transport'}`);
    }
    await sleep(50);
  }
  return { ok: false, started };
}

async function worker() {
  while (true) {
    const sequence = nextIteration++;
    if (sequence >= iterations || Date.now() >= deadline) return;
    if (await runOne(sequence)) completed++;
  }
}

await Promise.all(Array.from({ length: concurrency }, () => worker()));

const output = {
  mode: 'run',
  requested_iterations: iterations,
  completed_iterations: completed,
  failed_iterations: iterations - completed,
  limits: { concurrency, timeout_ms: timeoutMs, max_duration_ms: maxDurationMs },
  requests_total: requestsTotal,
  cleanup_poll_requests: cleanupPollRequests,
  http_statuses: Object.fromEntries([...httpStatuses].sort(([a], [b]) => Number(a) - Number(b))),
  retry_after_responses: retryAfterResponses,
  websocket_statuses: Object.fromEntries([...websocketStatuses].sort(([a], [b]) => Number(a) - Number(b))),
  websocket_retry_after_responses: websocketRetryAfterResponses,
  operations: Object.fromEntries([...metrics].map(([name, item]) => [name, {
    ok: item.ok,
    failed: item.failed,
    status_codes: Object.fromEntries([...item.statuses].sort(([a], [b]) => Number(a) - Number(b))),
    p50_ms: percentile(item.samples, 0.50),
    p95_ms: percentile(item.samples, 0.95),
    max_ms: item.samples.length ? Math.max(...item.samples) : 0,
  }])),
  failure_codes: Object.fromEntries([...failures].sort(([a], [b]) => a.localeCompare(b))),
};
console.log(JSON.stringify(output, null, 2));
if (completed !== iterations) process.exitCode = 1;

function endpoint(path) {
  return new URL(path, `${base.origin}/`).toString();
}

function websocketEndpoint(path) {
  const protocol = base.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${protocol}//${base.host}${path}`;
}

function inferOrigin(value) {
  try {
    const parsed = new URL(value);
    return isLocalTarget(parsed) ? 'http://localhost:8080' : 'https://pong.belacca.com';
  } catch {
    return 'http://localhost:8080';
  }
}

function normalizeHostname(hostname) {
  return hostname.toLowerCase().replace(/^\[|\]$/gu, '').replace(/\.$/u, '');
}

function isLocalTarget(url) {
  const hostname = normalizeHostname(url.hostname);
  if (hostname === 'localhost' || hostname.endsWith('.localhost')) return true;
  if (isIP(hostname) === 4) return hostname.startsWith('127.');
  if (isIP(hostname) === 6) return hostname === '::1' || hostname.startsWith('::ffff:127.');
  return false;
}

function parseFlags(argv) {
  const values = new Map();
  for (const arg of argv) {
    if (arg === '--dry-run' || arg === '--help') values.set(arg.slice(2), true);
    else if (arg.startsWith('--') && arg.includes('=')) {
      const [key, value] = arg.slice(2).split(/=(.*)/s, 2);
      values.set(key, value);
    }
  }
  return values;
}

function boundedNumber(value, fallback, minimum, maximum, name) {
  if (value === undefined) return fallback;
  if (value === '') throw new Error(`${name} must be a decimal integer`);
  if (typeof value !== 'string' && typeof value !== 'number') {
    throw new Error(`${name} must be a decimal integer`);
  }
  const text = String(value);
  if (!/^[0-9]+$/u.test(text)) throw new Error(`${name} must be a decimal integer`);
  const parsed = Number(text);
  if (!Number.isSafeInteger(parsed) || parsed < minimum || parsed > maximum) {
    throw new Error(`${name} must be between ${minimum} and ${maximum}`);
  }
  return parsed;
}

function statusClass(status) {
  return `${Math.floor(status / 100)}xx`;
}

function percentile(values, quantile) {
  if (!values.length) return 0;
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.min(sorted.length - 1, Math.floor((sorted.length - 1) * quantile))];
}

function sleep(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}
