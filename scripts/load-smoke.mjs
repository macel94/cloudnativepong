#!/usr/bin/env node
/**
 * Bounded local/disposable Pong journey smoke and load harness.
 *
 * Non-local targets require an explicit approved experiment marker. The
 * harness deliberately reports aggregate operation results only. Room IDs, names,
 * URLs, client addresses, tokens, and response bodies never appear in output.
 */
import process from 'node:process';
import WebSocket from 'ws';
import {
  isLocalTarget,
  parseBoundedInteger,
  parseExperimentURL,
  validateExperimentTarget,
} from './experiment-guard.mjs';

const MAX_ITERATIONS = 50;
const MAX_CONCURRENCY = 8;
const MAX_TIMEOUT_MS = 30_000;
const MAX_DURATION_MS = 180_000;
const MAX_ABORT_FAILURES = 20;
const DEFAULT_ITERATIONS = 3;
const DEFAULT_CONCURRENCY = 1;
const DEFAULT_TIMEOUT_MS = 10_000;
const DEFAULT_DURATION_MS = 60_000;
const DEFAULT_ABORT_FAILURES = 3;

const flags = parseFlags(process.argv.slice(2));
if (flags.has('help')) {
  console.log('usage: LOAD_SMOKE_BASE_URL=http://localhost:8080 node scripts/load-smoke.mjs [--dry-run]');
  console.log('non-local targets require PONG_EXPERIMENT_MODE, PONG_EXPERIMENT_APPROVED=1, and PONG_EXPERIMENT_TARGET=isolated');
  process.exit(0);
}
const dryRun = flags.has('dry-run');
const configuredBase = flags.get('base-url') || process.env.LOAD_SMOKE_BASE_URL;
const baseURL = configuredBase || 'http://localhost:8080';
let base;
let iterations;
let concurrency;
let timeoutMs;
let maxDurationMs;
let abortThreshold;
try {
  base = parseExperimentURL(baseURL);
  const target = validateExperimentTarget(base, process.env);
  if (!target.ok) throw new Error(target.message);
  iterations = parseBoundedInteger(flags.get('iterations') ?? process.env.LOAD_SMOKE_ITERATIONS, DEFAULT_ITERATIONS, 1, MAX_ITERATIONS, 'iterations');
  concurrency = parseBoundedInteger(flags.get('concurrency') ?? process.env.LOAD_SMOKE_CONCURRENCY, DEFAULT_CONCURRENCY, 1, MAX_CONCURRENCY, 'concurrency');
  timeoutMs = parseBoundedInteger(flags.get('timeout-ms') ?? process.env.LOAD_SMOKE_TIMEOUT_MS, DEFAULT_TIMEOUT_MS, 500, MAX_TIMEOUT_MS, 'timeout-ms');
  maxDurationMs = parseBoundedInteger(flags.get('max-duration-ms') ?? process.env.LOAD_SMOKE_MAX_DURATION_MS, DEFAULT_DURATION_MS, 1_000, MAX_DURATION_MS, 'max-duration-ms');
  abortThreshold = parseBoundedInteger(flags.get('abort-threshold') ?? process.env.LOAD_SMOKE_ABORT_THRESHOLD, DEFAULT_ABORT_FAILURES, 1, MAX_ABORT_FAILURES, 'abort-threshold');
} catch (error) {
  console.error(`load smoke configuration rejected: ${error.message}`);
  process.exit(2);
}
const origin = process.env.LOAD_SMOKE_ORIGIN || inferOrigin(baseURL);

if (dryRun) {
  console.log(JSON.stringify({
    mode: 'dry-run',
    operations: ['health', 'create', 'join', 'websocket', 'cleanup'],
    limits: { iterations, concurrency, timeout_ms: timeoutMs, max_duration_ms: maxDurationMs, abort_threshold: abortThreshold },
    output: 'aggregate counts, throughput, recovery, and latency percentiles only',
  }, null, 2));
  process.exit(0);
}

if (!configuredBase) {
  console.error('load smoke requires LOAD_SMOKE_BASE_URL, or use --dry-run');
  process.exit(2);
}

const startedAt = Date.now();
const deadline = startedAt + maxDurationMs;
const metrics = new Map(['health', 'create', 'join', 'websocket', 'cleanup'].map((name) => [name, { ok: 0, failed: 0, samples: [] }]));
const failures = new Map();
let completed = 0;
let attempted = 0;
let failureCount = 0;
let nextIteration = 0;
let aborted = false;
let cleanupFailures = 0;

function record(name, started, ok, failureCode = '') {
  const item = metrics.get(name);
  if (!item) return;
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

function markJourneyFailure() {
  failureCount++;
  if (failureCount >= abortThreshold) aborted = true;
}

async function request(path, options = {}) {
  const started = Date.now();
  const remainingUntilDeadline = deadline - started;
  if (remainingUntilDeadline <= 0) return { response: null, body: '', started, failureCode: 'deadline' };
  const remainingMs = Math.min(timeoutMs, remainingUntilDeadline);

  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), remainingMs);
  try {
    const response = await fetch(endpoint(path), { ...options, signal: controller.signal });
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
  let body;
  try {
    body = JSON.parse(result.body);
  } catch {
    record(name, result.started, false, `${name}_body`);
    return null;
  }
  if (!result.response.ok) {
    record(name, result.started, false, `${name}_http_${statusClass(result.response.status)}`);
    return null;
  }
  record(name, result.started, true);
  return body;
}

async function runOne(sequence) {
  let journeyOK = false;
  let cleanupOK = true;
  if (Date.now() >= deadline) {
    fail('deadline');
    markJourneyFailure();
    return false;
  }

  const healthResult = await request('/health');
  if (!healthResult.response || !healthResult.response.ok) {
    record('health', healthResult.started, false, `health_${healthResult.failureCode || 'unavailable'}`);
    markJourneyFailure();
    return false;
  }
  if (!healthResult.body.trim()) {
    record('health', healthResult.started, false, 'health_body');
    markJourneyFailure();
    return false;
  }
  record('health', healthResult.started, true);

  const room = await jsonRequest('create', '/api/rooms/create', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ name: `load-smoke-${process.pid}-${sequence}` }),
  });
  if (!room || typeof room.id !== 'string' || !/^[0-9a-f]{6}$/.test(room.id)) {
    fail('create_contract');
    markJourneyFailure();
    return false;
  }

  let sockets = [];
  let connectionAttempted = false;
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
      markJourneyFailure();
      return false;
    }

    connectionAttempted = true;
    const wsStarted = Date.now();
    const results = await Promise.allSettled([
      openPlayer(room.id),
      openPlayer(room.id),
    ]);
    sockets = results.filter((result) => result.status === 'fulfilled').map((result) => result.value);
    if (results.some((result) => result.status === 'rejected')) {
      record('websocket', wsStarted, false, 'websocket_contract');
      markJourneyFailure();
      return false;
    }
    record('websocket', wsStarted, true);
    journeyOK = true;
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
    cleanupOK = cleanup.ok;
    record('cleanup', cleanup.started, cleanup.ok, cleanup.ok ? '' : 'cleanup_timeout');
    if (!cleanup.ok) {
      cleanupFailures++;
      aborted = true;
      fail('cleanup_verification');
    }
  }
  return journeyOK && cleanupOK;
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
    socket.on('error', () => finish(new Error('socket')));
    socket.on('close', () => {
      if (!settled) finish(new Error('closed'));
    });
  });
}

async function waitForCleanup(roomID, started) {
  const cleanupDeadline = Math.min(deadline, Date.now() + timeoutMs);
  while (Date.now() < cleanupDeadline) {
    const result = await request('/api/rooms');
    if (result.response) {
      try {
        const rooms = JSON.parse(result.body);
        if (result.response.ok && Array.isArray(rooms) && !rooms.some((room) => room && room.id === roomID)) {
          return { ok: true, started };
        }
      } catch {
        // Retry until the bounded deadline.
      }
    }
    await sleep(50);
  }
  return { ok: false, started };
}

async function worker() {
  while (true) {
    if (aborted) return;
    const sequence = nextIteration++;
    if (sequence >= iterations || Date.now() >= deadline) return;
    attempted++;
    if (await runOne(sequence)) completed++;
  }
}

await Promise.all(Array.from({ length: concurrency }, () => worker()));

const elapsedMs = Math.min(Date.now() - startedAt, maxDurationMs);
const output = {
  mode: 'run',
  requested_iterations: iterations,
  attempted_iterations: attempted,
  completed_iterations: completed,
  failed_iterations: attempted - completed,
  failure_count: failureCount,
  limits: { concurrency, timeout_ms: timeoutMs, max_duration_ms: maxDurationMs, abort_threshold: abortThreshold },
  aborted,
  deadline_reached: Date.now() >= deadline,
  cleanup_verified: cleanupFailures === 0 && metrics.get('cleanup').failed === 0,
  throughput_per_second: elapsedMs > 0 ? Number((completed / (elapsedMs / 1000)).toFixed(3)) : 0,
  elapsed_ms: elapsedMs,
  operations: Object.fromEntries([...metrics].map(([name, item]) => [name, {
    ok: item.ok,
    failed: item.failed,
    p50_ms: percentile(item.samples, 0.50),
    p95_ms: percentile(item.samples, 0.95),
    max_ms: item.samples.length ? Math.max(...item.samples) : 0,
  }])),
  failure_codes: Object.fromEntries([...failures].sort(([a], [b]) => a.localeCompare(b))),
};
console.log(JSON.stringify(output, null, 2));
if (completed !== iterations || aborted || !output.cleanup_verified) process.exitCode = 1;

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
    return isLocalTarget(parsed) ? 'http://localhost:8080' : 'http://isolated.invalid';
  } catch {
    return 'http://localhost:8080';
  }
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
