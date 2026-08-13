#!/usr/bin/env node
/**
 * Public Pong synthetic: verify the public page/API, create and join one room,
 * establish the WebSocket-compatible default sessions, observe the playing state, and verify cleanup.
 *
 * The target is deliberately supplied by the caller. The scheduled GitHub
 * workflow supplies the canonical production URL; local invocations must set
 * SYNTHETIC_BASE_URL explicitly so they cannot accidentally exercise or mutate
 * production.
 */
import process from 'node:process';
import { writeFile } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import WebSocket from 'ws';

const DEFAULT_TIMEOUT_MS = 60_000;
const DEFAULT_REQUEST_TIMEOUT_MS = 20_000;
const CREATE_MAX_ATTEMPTS = 2;
const CREATE_RECOVERY_TIMEOUT_MS = 8_000;
const CREATE_RECOVERY_REQUEST_TIMEOUT_MS = 2_000;
const CREATE_RECOVERY_POLL_MS = 250;
const CREATE_ERROR_RECOVERY_TIMEOUT_MS = 2_000;
const CLEANUP_TIMEOUT_MS = 12_000;
const CLEANUP_POLL_MS = 250;
const MAX_RESPONSE_BYTES = 64 * 1024;
const ROOM_ID_PATTERN = /^[0-9a-f]{6}$/u;
const VALID_GAME_STATUSES = new Set(['waiting', 'playing', 'finished']);

export class SyntheticError extends Error {
  constructor(message, options = {}) {
    super(message, options);
    this.name = 'SyntheticError';
    this.retryable = options.retryable === true;
    this.ambiguous = options.ambiguous === true;
    this.status = options.status;
    this.stage = options.stage;
    this.code = options.code;
    this.cleanup = options.cleanup === true;
  }
}

export const JOURNEY_CONTRACT_VERSION = 'belacca.pong-slo-journey-result.v1';
export const JOURNEY_STAGES = Object.freeze([
  'homepage',
  'health',
  'room-list',
  'room-create',
  'room-join',
  'websocket-assignment',
  'playing-state',
  'cleanup',
]);

function boundedFailureCode(error, fallback = 'failed') {
  if (error instanceof SyntheticError && /^[a-z0-9_]+$/u.test(error.code || '')) return error.code;
  if (error instanceof SyntheticError && Number.isInteger(error.status)) return `http_${Math.floor(error.status / 100)}xx`;
  return fallback;
}

function stageError(error, stage, fallbackCode = 'failed') {
  if (error instanceof SyntheticError) {
    if (!error.stage) error.stage = stage;
    if (!error.code) error.code = boundedFailureCode(error, fallbackCode);
    return error;
  }
  return new SyntheticError('synthetic journey failed', {
    stage,
    code: fallbackCode,
    cause: error,
  });
}

async function atStage(stage, operation) {
  try {
    return await operation();
  } catch (error) {
    throw stageError(error, stage);
  }
}

function journeyResult({ good, durationMs, error = null, cleanupError = null }) {
  return {
    contract_version: JOURNEY_CONTRACT_VERSION,
    total: 1,
    good: good ? 1 : 0,
    failed: good ? 0 : 1,
    duration_ms: Math.max(0, durationMs),
    failure_stage: error?.stage || null,
    failure_code: error ? boundedFailureCode(error) : null,
    cleanup_failure_code: cleanupError ? boundedFailureCode(cleanupError, 'failed') : null,
  };
}

function usage() {
  console.log('usage: SYNTHETIC_BASE_URL=https://... node scripts/synthetic-check.mjs [--dry-run]');
}

async function writeEvidence(path, status, startedAt, completedAt) {
  if (typeof path !== 'string' || path.trim() === '') return;
  // This artifact is intentionally aggregate-only. Never include the target,
  // room identifiers, names, response bodies, tokens, or error text.
  const evidence = {
    status,
    started_at: new Date(startedAt).toISOString().replace('.000Z', 'Z'),
    completed_at: new Date(completedAt).toISOString().replace('.000Z', 'Z'),
    duration_ms: Math.max(0, completedAt - startedAt),
  };
  await writeFile(path, `${JSON.stringify(evidence, null, 2)}\n`, { mode: 0o600 });
}

function positiveInteger(raw, name, fallback, maximum) {
  const value = raw === undefined || raw === '' ? fallback : Number(raw);
  if (!Number.isSafeInteger(value) || value <= 0 || value > maximum) {
    throw new SyntheticError(`${name} must be an integer between 1 and ${maximum}`);
  }
  return value;
}

/**
 * Validate and normalize a target URL. A path prefix is supported so the
 * runner can exercise an ingress mounted below the origin root.
 */
export function parseBaseURL(raw) {
  if (typeof raw !== 'string' || raw.trim() === '') {
    throw new SyntheticError('SYNTHETIC_BASE_URL must be supplied for an executing check');
  }

  let base;
  try {
    base = new URL(raw.trim());
  } catch {
    throw new SyntheticError('SYNTHETIC_BASE_URL must be a valid URL');
  }
  if (!['http:', 'https:'].includes(base.protocol)) {
    throw new SyntheticError('SYNTHETIC_BASE_URL must use http or https');
  }
  if (!base.hostname || base.username || base.password || base.search || base.hash) {
    throw new SyntheticError('SYNTHETIC_BASE_URL must not contain credentials, a query, or a fragment');
  }

  // URL resolution treats a trailing slash as a directory. Preserve a
  // configured path prefix while making that behavior explicit.
  base.pathname = `${base.pathname.replace(/\/{2,}/gu, '/').replace(/\/$/u, '')}/`;
  return base;
}

function parseOrigin(raw, fallback) {
  if (typeof raw !== 'string' || raw.trim() === '') return fallback;
  let origin;
  try {
    origin = new URL(raw.trim());
  } catch {
    throw new SyntheticError('SYNTHETIC_ORIGIN must be a valid URL');
  }
  if (!['http:', 'https:'].includes(origin.protocol) || !origin.hostname ||
      origin.username || origin.password || origin.pathname !== '/' ||
      origin.search || origin.hash) {
    throw new SyntheticError('SYNTHETIC_ORIGIN must be an origin without a path, query, or fragment');
  }
  return origin.origin;
}

function endpoint(base, path) {
  const relativePath = path.replace(/^\/+/, '');
  return new URL(relativePath, base).toString();
}

function websocketURL(httpURL) {
  const value = new URL(httpURL);
  value.protocol = value.protocol === 'https:' ? 'wss:' : 'ws:';
  return value.toString();
}

function remaining(deadline) {
  const value = deadline - Date.now();
  if (value <= 0) throw new SyntheticError('synthetic check exceeded its overall timeout');
  return value;
}

function safeError(error, fallback = 'request failed') {
  if (error instanceof SyntheticError) return error;
  if (error?.name === 'AbortError') return new SyntheticError('request timed out');
  return new SyntheticError(fallback);
}

function authHeaders(env) {
  const token = typeof env.SYNTHETIC_AUTH_TOKEN === 'string' ? env.SYNTHETIC_AUTH_TOKEN : '';
  return token ? { authorization: `Bearer ${token}` } : {};
}

function requestHeaders(env, extra = {}) {
  return {
    accept: 'application/json, text/plain, text/html;q=0.9',
    'user-agent': 'cloudnativepong-synthetic/1',
    ...authHeaders(env),
    ...extra,
  };
}

async function readLimitedBody(response, label) {
  if (!response.body) return '';
  const chunks = [];
  let size = 0;
  try {
    for await (const chunk of response.body) {
      const buffer = Buffer.from(chunk);
      size += buffer.byteLength;
      if (size > MAX_RESPONSE_BYTES) {
        throw new SyntheticError(`${label} response exceeded ${MAX_RESPONSE_BYTES} bytes`);
      }
      chunks.push(buffer);
    }
  } catch (error) {
    if (error instanceof SyntheticError) throw error;
    throw new SyntheticError(`${label} response could not be read`, {
      retryable: true,
      ambiguous: true,
      cause: error,
    });
  }
  return Buffer.concat(chunks, size).toString('utf8');
}

async function requestBody({ fetchImpl, url, init, label, deadline, requestTimeoutMs }) {
  const timeoutMs = Math.min(requestTimeoutMs, remaining(deadline));
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const response = await fetchImpl(url, {
      ...init,
      redirect: 'error',
      signal: controller.signal,
    });
    // Keep the same abort signal active while consuming the body. A proxy can
    // deliver headers and then stall indefinitely; the synthetic must still
    // finish within its per-request bound.
    const body = await readLimitedBody(response, label);
    return { response, body };
  } catch (error) {
    if (controller.signal.aborted) {
      throw new SyntheticError(`${label} timed out after ${timeoutMs}ms`, {
        retryable: true,
        ambiguous: true,
        cause: error,
      });
    }
    if (error instanceof SyntheticError) throw error;
    throw new SyntheticError(`${label} request failed`, {
      retryable: true,
      ambiguous: true,
      cause: error,
    });
  } finally {
    clearTimeout(timer);
  }
}

export function requireStatus(response, label, expected = 200) {
  if (response.status !== expected) {
    // 429 is an explicit overload decision. Retrying it from this layer would
    // amplify load against the same single-writer API; callers must honor the
    // server's Retry-After policy or surface the failure.
    const retryable = response.status === 408 || response.status === 425 ||
      response.status >= 500;
    throw new SyntheticError(`${label} returned HTTP ${response.status}; expected HTTP ${expected}`, {
      retryable,
      status: response.status,
    });
  }
}

async function getText(client, path, label) {
  const { response, body } = await requestBody({
    ...client,
    url: endpoint(client.base, path),
    init: { method: 'GET', headers: requestHeaders(client.env) },
    label,
  });
  requireStatus(response, label);
  return body;
}

async function getRooms(client) {
  const { response, body } = await requestBody({
    ...client,
    url: endpoint(client.base, '/api/rooms'),
    init: { method: 'GET', headers: requestHeaders(client.env) },
    label: 'room list',
  });
  requireStatus(response, 'room list');
  let rooms;
  try {
    rooms = JSON.parse(body);
  } catch {
    throw new SyntheticError('room list returned invalid JSON');
  }
  if (!Array.isArray(rooms)) throw new SyntheticError('room list returned a non-array JSON value');
  return rooms;
}

async function postJSON(client, path, payload, label) {
  const { response, body } = await requestBody({
    ...client,
    url: endpoint(client.base, path),
    init: {
      method: 'POST',
      headers: requestHeaders(client.env, { 'content-type': 'application/json' }),
      body: JSON.stringify(payload),
    },
    label,
  });
  requireStatus(response, label);
  try {
    return JSON.parse(body);
  } catch (error) {
    throw new SyntheticError(`${label} returned invalid JSON`, {
      retryable: true,
      ambiguous: true,
      cause: error,
    });
  }
}

function isRetryable(error) {
  return error instanceof SyntheticError && error.retryable;
}

function sleep(delayMs) {
  return new Promise((resolve) => setTimeout(resolve, delayMs));
}

function roomWithName(room, name) {
  return room && typeof room === 'object' && room.name === name &&
    ROOM_ID_PATTERN.test(room.id || '');
}

async function collectMatchingRooms(client, name, rememberRoom) {
  try {
    const rooms = await getRooms(client);
    const matches = rooms.filter((room) => roomWithName(room, name));
    for (const room of matches) rememberRoom(room);
    return matches;
  } catch {
    // This is supplementary reconciliation. The primary journey result must
    // not become a false negative because a best-effort room list is delayed.
    return [];
  }
}

async function recoverCreatedRoom(client, name, rememberRoom, recoveryTimeoutMs) {
  const deadline = Math.min(
    client.deadline,
    Date.now() + recoveryTimeoutMs,
  );
  const recoveryClient = {
    ...client,
    deadline,
    requestTimeoutMs: Math.max(client.requestTimeoutMs, CREATE_RECOVERY_REQUEST_TIMEOUT_MS),
  };
  while (Date.now() < deadline) {
    const matches = await collectMatchingRooms(recoveryClient, name, rememberRoom);
    if (matches.length > 0) return matches;
    const delay = Math.min(CREATE_RECOVERY_POLL_MS, deadline - Date.now());
    if (delay > 0) await sleep(delay);
  }
  return [];
}

async function createRoom(client, name, rememberRoom) {
  let lastError;
  let retried = false;
  const createClient = client.createRequestTimeoutMs === undefined
    ? client
    : { ...client, requestTimeoutMs: client.createRequestTimeoutMs };

  for (let attempt = 1; attempt <= CREATE_MAX_ATTEMPTS; attempt += 1) {
    try {
      const room = await postJSON(createClient, '/api/rooms/create', { name }, 'room creation');
      rememberRoom(room);
      // If an earlier request timed out after the server accepted it, a second
      // room may become visible shortly after the successful retry. Track it
      // so the finalizer can clean both rooms rather than leaking capacity.
      if (retried) await collectMatchingRooms(client, name, rememberRoom);
      return room;
    } catch (error) {
      lastError = error;
      if (!isRetryable(error)) throw error;

      const recovered = await recoverCreatedRoom(
        client,
        name,
        rememberRoom,
        error.ambiguous
          ? CREATE_RECOVERY_TIMEOUT_MS
          : (error.status >= 500 ? CREATE_ERROR_RECOVERY_TIMEOUT_MS : CREATE_RECOVERY_POLL_MS),
      );
      if (recovered.length > 0) {
        console.warn('room creation response was delayed; recovered the created room by name');
        return recovered[0];
      }
      if (attempt === CREATE_MAX_ATTEMPTS) break;

      retried = true;
      const delay = Math.min(500 * 2 ** (attempt - 1), remaining(client.deadline));
      console.warn(`room creation attempt ${attempt} was transient (${error.message}); retrying in ${delay}ms`);
      await sleep(delay);
    }
  }

  throw lastError;
}

function validateHomepage(body) {
  if (!/<title>Cloud Native Pong\s*[—-]/u.test(body) || !body.includes('id="playerName"')) {
    throw new SyntheticError('homepage did not contain the expected Pong application markers');
  }
}

function validateRoom(room) {
  if (!room || typeof room !== 'object' || !ROOM_ID_PATTERN.test(room.id || '')) {
    throw new SyntheticError('room creation returned an invalid room identifier');
  }
  if (room.status !== 'waiting' || room.players !== 1) {
    throw new SyntheticError('room creation returned an unexpected initial state');
  }
}

function validateJoin(joined, roomID) {
  if (!joined || typeof joined !== 'object' || joined.room_id !== roomID ||
      joined.ws_path !== `/rooms/${roomID}/ws`) {
    throw new SyntheticError('room join returned an unexpected connection contract');
  }
}

function waitForPlayer(socket, timeoutMs) {
  return new Promise((resolve, reject) => {
    let joinedPlayer = null;
    let playing = false;
    let settled = false;
    const timer = setTimeout(() => finishReject(new SyntheticError('WebSocket-compatible player journey timed out', {
      stage: joinedPlayer === null ? 'websocket-assignment' : 'playing-state',
      code: joinedPlayer === null ? 'assignment_timeout' : 'playing_timeout',
    })), timeoutMs);

    function finishResolve(value) {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      socket.removeListener('message', onMessage);
      socket.removeListener('open', onOpen);
      socket.removeListener('close', onClose);
      resolve(value);
    }

    function finishReject(error) {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      socket.removeListener('message', onMessage);
      socket.removeListener('open', onOpen);
      socket.removeListener('close', onClose);
      reject(error instanceof SyntheticError ? error : new SyntheticError('WebSocket-compatible player journey failed'));
    }

    function onOpen() {
      try {
        socket.send(JSON.stringify({ type: 'proxy-ready' }));
      } catch {
        finishReject(new SyntheticError('WebSocket readiness message could not be sent'));
      }
    }

    function onMessage(payload) {
      let message;
      try {
        message = JSON.parse(payload.toString());
      } catch {
        finishReject(new SyntheticError('WebSocket returned invalid JSON', { stage: 'playing-state', code: 'invalid_json' }));
        return;
      }
      if (!message || typeof message !== 'object') {
        finishReject(new SyntheticError('WebSocket returned an invalid message', { stage: 'playing-state', code: 'invalid_message' }));
        return;
      }
      if (message.type === 'error') {
        finishReject(new SyntheticError('WebSocket returned an application error', { stage: 'playing-state', code: 'application_error' }));
        return;
      }
      if (message.type === 'joined') {
        if (!Number.isInteger(message.player) || ![1, 2].includes(message.player)) {
          finishReject(new SyntheticError('WebSocket returned an invalid player assignment', { stage: 'websocket-assignment', code: 'invalid_assignment' }));
          return;
        }
        joinedPlayer = message.player;
      }
      if (message.type === 'state') {
        const state = message.state;
        if (!state || typeof state !== 'object' || !VALID_GAME_STATUSES.has(state.status)) {
          finishReject(new SyntheticError('WebSocket returned an invalid game state', { stage: 'playing-state', code: 'invalid_state' }));
          return;
        }
        if (state.status === 'playing') playing = true;
      }
      if (joinedPlayer !== null && playing) finishResolve({ player: joinedPlayer });
    }

    function onError() {
      finishReject(new SyntheticError('WebSocket connection failed', { stage: 'websocket-assignment', code: 'connection_failed' }));
    }

    function onClose(code) {
      if (!settled) finishReject(new SyntheticError('WebSocket closed before the playing state', {
        stage: joinedPlayer === null ? 'websocket-assignment' : 'playing-state',
        code: 'closed_before_ready',
      }));
    }

    // Keep the error listener installed after resolution. ws emits errors
    // asynchronously during forced cleanup, and an unhandled error would
    // otherwise obscure the actual synthetic result.
    socket.on('error', onError);
    socket.on('open', onOpen);
    socket.on('message', onMessage);
    socket.on('close', onClose);
  });
}

function createSocket(url, { env, origin, deadline, requestTimeoutMs, WebSocketImpl = WebSocket }) {
  const timeoutMs = Math.min(requestTimeoutMs, remaining(deadline));
  try {
    return {
      socket: new WebSocketImpl(url, {
        headers: { ...authHeaders(env), origin },
        handshakeTimeout: timeoutMs,
        maxPayload: MAX_RESPONSE_BYTES,
      }),
      timeoutMs,
    };
  } catch {
    throw new SyntheticError('WebSocket connection could not be created');
  }
}

function waitForOpen(socket, timeoutMs) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const timer = setTimeout(() => finishReject(new SyntheticError('WebSocket cleanup connection timed out')), timeoutMs);

    function finishResolve() {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      socket.removeListener('open', onOpen);
      socket.removeListener('close', onClose);
      resolve();
    }

    function finishReject(error) {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      socket.removeListener('open', onOpen);
      socket.removeListener('close', onClose);
      reject(error instanceof SyntheticError ? error : new SyntheticError('WebSocket cleanup connection failed'));
    }

    function onOpen() {
      try {
        socket.send(JSON.stringify({ type: 'proxy-ready' }));
        finishResolve();
      } catch {
        finishReject(new SyntheticError('WebSocket cleanup readiness message could not be sent'));
      }
    }

    function onClose(code) {
      finishReject(new SyntheticError(`WebSocket cleanup connection closed before opening (code ${code})`));
    }

    // Keep the error listener through forced cleanup; ws may emit an error
    // after terminate(), and an unhandled EventEmitter error would hide the
    // original synthetic failure.
    socket.on('error', () => finishReject(new SyntheticError('WebSocket cleanup connection failed')));
    socket.once('open', onOpen);
    socket.once('close', onClose);
  });
}

function openPlayer(url, options) {
  const { socket, timeoutMs } = createSocket(url, options);
  const ready = waitForPlayer(socket, timeoutMs);
  return { socket, ready };
}

function openCleanupPlayer(url, options) {
  const { socket, timeoutMs } = createSocket(url, options);
  const ready = waitForOpen(socket, timeoutMs);
  return { socket, ready };
}

async function closeSocket(socket) {
  if (!socket || socket.readyState === 3) return;
  await new Promise((resolve) => {
    let settled = false;
    const timer = setTimeout(() => {
      try { socket.terminate(); } catch { /* already closed */ }
      finish();
    }, 750);
    function finish() {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      socket.removeListener('close', finish);
      resolve();
    }
    socket.once('close', finish);
    try {
      if (socket.readyState === 1 || socket.readyState === 2) socket.close();
      else socket.terminate();
    } catch {
      finish();
    }
  });
}

async function triggerRoomCleanup(client, roomID, origin, WebSocketImpl, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  let player;
  try {
    player = openCleanupPlayer(websocketURL(endpoint(client.base, `/rooms/${roomID}/ws`)), {
      env: client.env,
      origin,
      deadline,
      requestTimeoutMs: client.requestTimeoutMs,
      WebSocketImpl,
    });
    await player.ready;
  } catch {
    // Cleanup is best-effort here. The subsequent room-list poll is the
    // authoritative result and reports a useful failure if the room remains.
  } finally {
    if (player) await closeSocket(player.socket);
  }
}

async function waitForCleanup(client, roomID, timeoutMs, pollMs) {
  // Cleanup is deliberately independent from the main journey deadline. A
  // WebSocket timeout must not prevent us from checking that the room was
  // eventually removed.
  const deadline = Date.now() + timeoutMs;
  const cleanupClient = { ...client, deadline };
  while (true) {
    if (Date.now() >= deadline) {
      throw new SyntheticError('synthetic check exceeded its overall timeout');
    }
    try {
      const rooms = await getRooms(cleanupClient);
      if (!rooms.some((room) => room && room.id === roomID)) return;
    } catch (error) {
      if (!isRetryable(error)) throw error;
      if (Date.now() >= deadline) {
        throw new SyntheticError('synthetic check exceeded its overall timeout', { cause: error });
      }
    }
    const delay = Math.min(pollMs, remaining(deadline));
    await sleep(delay);
  }
}

function makeClient({ base, env, fetchImpl, deadline, requestTimeoutMs, createRequestTimeoutMs }) {
  return { base, env, fetchImpl, deadline, requestTimeoutMs, createRequestTimeoutMs };
}

/**
 * Execute the complete check. It returns a small aggregate result and never
 * returns room IDs, response bodies, tokens, or client addresses.
 */
export async function runSynthetic({
  env = process.env,
  baseURL = env.SYNTHETIC_BASE_URL,
  origin = env.SYNTHETIC_ORIGIN,
  timeoutMs = positiveInteger(env.SYNTHETIC_TIMEOUT_MS, 'SYNTHETIC_TIMEOUT_MS', DEFAULT_TIMEOUT_MS, 120_000),
  requestTimeoutMs = positiveInteger(env.SYNTHETIC_REQUEST_TIMEOUT_MS, 'SYNTHETIC_REQUEST_TIMEOUT_MS', DEFAULT_REQUEST_TIMEOUT_MS, 60_000),
  createRequestTimeoutMs = requestTimeoutMs,
  fetchImpl = globalThis.fetch,
  WebSocketImpl = WebSocket,
  cleanupTimeoutMs = CLEANUP_TIMEOUT_MS,
  cleanupPollMs = CLEANUP_POLL_MS,
  dryRun = false,
} = {}) {
  if (dryRun && (typeof baseURL !== 'string' || baseURL.trim() === '')) {
    console.log('synthetic dry-run: set SYNTHETIC_BASE_URL to execute the public check');
    return { dryRun: true };
  }

  const base = parseBaseURL(baseURL);
  const syntheticOrigin = parseOrigin(origin, base.origin);
  const cleanupTimeout = positiveInteger(cleanupTimeoutMs, 'cleanupTimeoutMs', CLEANUP_TIMEOUT_MS, 120_000);
  const cleanupPoll = positiveInteger(cleanupPollMs, 'cleanupPollMs', CLEANUP_POLL_MS, 60_000);
  const createRequestTimeout = positiveInteger(
    createRequestTimeoutMs,
    'createRequestTimeoutMs',
    requestTimeoutMs,
    60_000,
  );
  const deadline = Date.now() + timeoutMs;

  if (dryRun) {
    console.log(`synthetic dry-run: would check ${base.origin}${base.pathname}`);
    console.log('synthetic dry-run: homepage, health, room list/create/join, two WebSocket-compatible sessions, and cleanup');
    return { dryRun: true, baseURL: `${base.origin}${base.pathname}` };
  }

  if (typeof fetchImpl !== 'function') throw new SyntheticError('fetch is not available');

  const client = makeClient({
    base,
    env,
    fetchImpl,
    deadline,
    requestTimeoutMs,
    createRequestTimeoutMs: createRequestTimeout,
  });
  const startedAt = Date.now();
  let roomID = null;
  const roomIDs = new Set();
  let roomName = null;
  let sockets = [];
  let primaryError = null;
  let cleanupError = null;
  const rememberRoom = (room) => {
    if (!room || typeof room !== 'object' || !ROOM_ID_PATTERN.test(room.id || '')) return;
    roomIDs.add(room.id);
    if (roomID === null) roomID = room.id;
  };

  try {
    await atStage('homepage', async () => {
      const homepage = await getText(client, '/', 'homepage');
      validateHomepage(homepage);
    });

    await atStage('health', async () => {
      const health = (await getText(client, '/health', 'health')).trim();
      if (health !== 'ok') throw new SyntheticError('health returned an unexpected body', { code: 'unexpected_body' });
    });

    await atStage('room-list', () => getRooms(client));

    roomName = `synthetic-${Date.now().toString(36)}-${randomUUID().slice(0, 8)}`;
    const room = await atStage('room-create', async () => {
      const created = await createRoom(client, roomName, rememberRoom);
      // Preserve a valid identifier before checking the rest of the response
      // so a malformed response cannot strand a successfully-created room.
      rememberRoom(created);
      validateRoom(created);
      return created;
    });

    await atStage('room-join', async () => {
      const joined = await postJSON(client, '/api/rooms/join', { room_id: roomID }, 'room join');
      validateJoin(joined, roomID);
    });

    const wsURL = websocketURL(endpoint(base, `/rooms/${roomID}/ws`));
    const playerOptions = {
      env,
      origin: syntheticOrigin,
      deadline,
      requestTimeoutMs,
      WebSocketImpl,
    };
    await atStage('websocket-assignment', async () => {
      sockets.push(openPlayer(wsURL, playerOptions));
      sockets.push(openPlayer(wsURL, playerOptions));
      const assignments = await Promise.all(sockets.map((player) => player.ready));
      const players = new Set(assignments.map(({ player }) => player));
      if (players.size !== 2 || !players.has(1) || !players.has(2)) {
        throw new SyntheticError('WebSocket-compatible journey did not assign one Player 1 and one Player 2', {
          code: 'duplicate_assignment',
        });
      }
    });
  } catch (error) {
    primaryError = safeError(error, 'synthetic journey failed');
  } finally {
    await Promise.all(sockets.map(({ socket }) => closeSocket(socket)));
    // Reconcile once more after an ambiguous create. This catches a delayed
    // first request that completed after a retry and prevents leaked rooms.
    if (roomName !== null) {
      await collectMatchingRooms(
        { ...client, deadline: Date.now() + cleanupTimeout },
        roomName,
        rememberRoom,
      );
    }
    const cleanupIDs = [...roomIDs];
    const roomsNeedingForcedCleanup = cleanupIDs.filter((id) => primaryError || id !== roomID);
    const cleanupClient = {
      ...client,
      deadline: Date.now() + cleanupTimeout,
    };
    await Promise.all(roomsNeedingForcedCleanup.map((id) => triggerRoomCleanup(
      cleanupClient,
      id,
      syntheticOrigin,
      WebSocketImpl,
      cleanupTimeout,
    )));
    for (const id of cleanupIDs) {
      try {
        await waitForCleanup(client, id, cleanupTimeout, cleanupPoll);
      } catch (error) {
        const failure = stageError(error, 'cleanup', 'cleanup_failed');
        cleanupError = cleanupError
          ? new SyntheticError('room cleanup verification failed', {
            stage: 'cleanup',
            code: 'cleanup_failed',
            cause: failure,
          })
          : failure;
      }
    }
  }

  const durationMs = Date.now() - startedAt;
  const result = journeyResult({
    good: !primaryError && !cleanupError,
    durationMs,
    error: primaryError || cleanupError,
    cleanupError,
  });
  if (primaryError && cleanupError) {
    primaryError.message = `${primaryError.message}; cleanup verification failed`;
  }
  if (primaryError) {
    primaryError.result = result;
    throw primaryError;
  }
  if (cleanupError) {
    cleanupError.result = result;
    throw cleanupError;
  }

  console.log(`synthetic passed: homepage, health, room CRUD, two-player WebSocket-compatible state, and cleanup (${durationMs}ms)`);
  console.log(`synthetic_result ${JSON.stringify(result)}`);
  return { dryRun: false, durationMs: result.duration_ms, ...result };
}

export async function main(argv = process.argv.slice(2), env = process.env) {
  const args = new Set(argv);
  if (args.has('--help')) {
    usage();
    return 0;
  }
  const dryRun = args.has('--dry-run');
  const startedAt = Date.now();
  try {
    await runSynthetic({ env, dryRun });
    await writeEvidence(env.SYNTHETIC_EVIDENCE_FILE, 'passed', startedAt, Date.now());
    return 0;
  } catch (error) {
    const completedAt = Date.now();
    try {
      await writeEvidence(env.SYNTHETIC_EVIDENCE_FILE, 'failed', startedAt, completedAt);
    } catch {
      // Preserve the primary failed-check result. The promotion workflow must
      // fail closed when its required evidence file is absent.
    }
    const message = error instanceof SyntheticError ? error.message : 'synthetic check failed';
    const stage = error instanceof SyntheticError && error.stage ? ` stage=${error.stage}` : '';
    const code = error instanceof SyntheticError && error.code ? ` code=${boundedFailureCode(error)}` : '';
    console.error(`synthetic failed${stage}${code}: ${message}`);
    if (error?.result) console.log(`synthetic_result ${JSON.stringify(error.result)}`);
    return 1;
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const exitCode = await main();
  process.exitCode = exitCode;
}
