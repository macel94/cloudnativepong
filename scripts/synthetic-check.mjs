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
import { randomUUID } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import WebSocket from 'ws';

const DEFAULT_TIMEOUT_MS = 20_000;
const DEFAULT_REQUEST_TIMEOUT_MS = 8_000;
const CLEANUP_TIMEOUT_MS = 12_000;
const CLEANUP_POLL_MS = 250;
const MAX_RESPONSE_BYTES = 64 * 1024;
const ROOM_ID_PATTERN = /^[0-9a-f]{6}$/u;
const VALID_GAME_STATUSES = new Set(['waiting', 'playing', 'finished']);

export class SyntheticError extends Error {
  constructor(message, options) {
    super(message, options);
    this.name = 'SyntheticError';
  }
}

function usage() {
  console.log('usage: SYNTHETIC_BASE_URL=https://... node scripts/synthetic-check.mjs [--dry-run]');
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
    throw new SyntheticError(`${label} response could not be read`);
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
      throw new SyntheticError(`${label} timed out after ${timeoutMs}ms`);
    }
    if (error instanceof SyntheticError) throw error;
    throw new SyntheticError(`${label} request failed`);
  } finally {
    clearTimeout(timer);
  }
}

function requireStatus(response, label, expected = 200) {
  if (response.status !== expected) {
    throw new SyntheticError(`${label} returned HTTP ${response.status}; expected HTTP ${expected}`);
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
  } catch {
    throw new SyntheticError(`${label} returned invalid JSON`);
  }
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
    const timer = setTimeout(() => finishReject(new SyntheticError('WebSocket-compatible player journey timed out')), timeoutMs);

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
        finishReject(new SyntheticError('WebSocket returned invalid JSON'));
        return;
      }
      if (!message || typeof message !== 'object') {
        finishReject(new SyntheticError('WebSocket returned an invalid message'));
        return;
      }
      if (message.type === 'error') {
        finishReject(new SyntheticError('WebSocket returned an application error'));
        return;
      }
      if (message.type === 'joined') {
        if (!Number.isInteger(message.player) || ![1, 2].includes(message.player)) {
          finishReject(new SyntheticError('WebSocket returned an invalid player assignment'));
          return;
        }
        joinedPlayer = message.player;
      }
      if (message.type === 'state') {
        const state = message.state;
        if (!state || typeof state !== 'object' || !VALID_GAME_STATUSES.has(state.status)) {
          finishReject(new SyntheticError('WebSocket returned an invalid game state'));
          return;
        }
        if (state.status === 'playing') playing = true;
      }
      if (joinedPlayer !== null && playing) finishResolve({ player: joinedPlayer });
    }

    function onError() {
      finishReject(new SyntheticError('WebSocket connection failed'));
    }

    function onClose(code) {
      if (!settled) finishReject(new SyntheticError(`WebSocket closed before the playing state (code ${code})`));
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
      if (Date.now() >= deadline) {
        throw new SyntheticError('synthetic check exceeded its overall timeout', { cause: error });
      }
      throw error;
    }
    const delay = Math.min(pollMs, remaining(deadline));
    await new Promise((resolve) => setTimeout(resolve, delay));
  }
}

function makeClient({ base, env, fetchImpl, deadline, requestTimeoutMs }) {
  return { base, env, fetchImpl, deadline, requestTimeoutMs };
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
  const deadline = Date.now() + timeoutMs;

  if (dryRun) {
    console.log(`synthetic dry-run: would check ${base.origin}${base.pathname}`);
    console.log('synthetic dry-run: homepage, health, room list/create/join, two WebSocket-compatible sessions, and cleanup');
    return { dryRun: true, baseURL: `${base.origin}${base.pathname}` };
  }

  if (typeof fetchImpl !== 'function') throw new SyntheticError('fetch is not available');

  const client = makeClient({ base, env, fetchImpl, deadline, requestTimeoutMs });
  const startedAt = Date.now();
  let roomID = null;
  let sockets = [];
  let primaryError = null;
  let cleanupError = null;

  try {
    const homepage = await getText(client, '/', 'homepage');
    validateHomepage(homepage);

    const health = (await getText(client, '/health', 'health')).trim();
    if (health !== 'ok') throw new SyntheticError('health returned an unexpected body');

    await getRooms(client);

    const roomName = `synthetic-${Date.now().toString(36)}-${randomUUID().slice(0, 8)}`;
    const room = await postJSON(client, '/api/rooms/create', { name: roomName }, 'room creation');
    // Preserve a valid identifier before checking the rest of the response so
    // a malformed status/players field cannot strand a successfully-created
    // room.
    if (room && typeof room === 'object' && ROOM_ID_PATTERN.test(room.id || '')) roomID = room.id;
    validateRoom(room);

    const joined = await postJSON(client, '/api/rooms/join', { room_id: roomID }, 'room join');
    validateJoin(joined, roomID);

    const wsURL = websocketURL(endpoint(base, `/rooms/${roomID}/ws`));
    const playerOptions = {
      env,
      origin: syntheticOrigin,
      deadline,
      requestTimeoutMs,
      WebSocketImpl,
    };
    sockets.push(openPlayer(wsURL, playerOptions));
    sockets.push(openPlayer(wsURL, playerOptions));
    const assignments = await Promise.all(sockets.map((player) => player.ready));
    const players = new Set(assignments.map(({ player }) => player));
    if (players.size !== 2 || !players.has(1) || !players.has(2)) {
      throw new SyntheticError('WebSocket-compatible journey did not assign one Player 1 and one Player 2');
    }
  } catch (error) {
    primaryError = safeError(error, 'synthetic journey failed');
  } finally {
    await Promise.all(sockets.map(({ socket }) => closeSocket(socket)));
    if (roomID !== null && primaryError) {
      await triggerRoomCleanup(client, roomID, syntheticOrigin, WebSocketImpl, cleanupTimeout);
    }
    if (roomID !== null) {
      try {
        await waitForCleanup(client, roomID, cleanupTimeout, cleanupPoll);
      } catch (error) {
        cleanupError = safeError(error, 'room cleanup verification failed');
      }
    }
  }

  if (primaryError && cleanupError) {
    throw new SyntheticError(`${primaryError.message}; ${cleanupError.message}`);
  }
  if (primaryError) throw primaryError;
  if (cleanupError) throw cleanupError;

  const durationMs = Date.now() - startedAt;
  console.log(`synthetic passed: homepage, health, room CRUD, two-player WebSocket-compatible state, and cleanup (${durationMs}ms)`);
  return { dryRun: false, durationMs };
}

export async function main(argv = process.argv.slice(2), env = process.env) {
  const args = new Set(argv);
  if (args.has('--help')) {
    usage();
    return 0;
  }
  const dryRun = args.has('--dry-run');
  try {
    await runSynthetic({ env, dryRun });
    return 0;
  } catch (error) {
    const message = error instanceof SyntheticError ? error.message : 'synthetic check failed';
    console.error(`synthetic failed: ${message}`);
    return 1;
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const exitCode = await main();
  process.exitCode = exitCode;
}
