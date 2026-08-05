#!/usr/bin/env node
/**
 * Public Pong synthetic: HTTP health/list, create + join, then two WebSockets.
 * Endpoint and optional auth are supplied out-of-band through the environment.
 */
import process from 'node:process';
import WebSocket from 'ws';

const args = new Set(process.argv.slice(2));
const configuredBaseURL = process.env.SYNTHETIC_BASE_URL;
const baseURL = configuredBaseURL || 'https://example.invalid';
const timeoutMs = Number(process.env.SYNTHETIC_TIMEOUT_MS || 15000);

function usage() {
  console.error('usage: SYNTHETIC_BASE_URL=https://... node scripts/synthetic-check.mjs [--dry-run]');
  process.exit(2);
}

if (args.has('--help')) usage();
if (!configuredBaseURL && !args.has('--dry-run')) {
  throw new Error('SYNTHETIC_BASE_URL must be supplied out-of-band');
}
const base = new URL(baseURL);
if (!['http:', 'https:'].includes(base.protocol)) throw new Error('base URL must use http or https');
base.pathname = base.pathname.replace(/\/$/, '');
const origin = process.env.SYNTHETIC_ORIGIN ||
  (['127.0.0.1', 'localhost', '[::1]'].includes(base.hostname)
    ? 'http://localhost:8080'
    : 'https://pong.belacca.com');

function endpoint(path) {
  return new URL(path, `${base.origin}/`).toString();
}

function authHeaders() {
  const token = process.env.SYNTHETIC_AUTH_TOKEN;
  return token ? { authorization: `Bearer ${token}` } : {};
}

if (args.has('--dry-run')) {
  console.log(`would GET ${endpoint('/health')}`);
  console.log(`would GET ${endpoint('/api/rooms')}`);
  console.log(`would POST ${endpoint('/api/rooms/create')} and /api/rooms/join`);
  const wsProtocol = base.protocol === 'https:' ? 'wss:' : 'ws:';
  console.log(`would open two ${wsProtocol}//${base.host}/rooms/<id>/ws connections`);
  process.exit(0);
}

const controller = new AbortController();
const abortTimer = setTimeout(() => controller.abort(), timeoutMs);
try {
  const health = await fetch(endpoint('/health'), { headers: authHeaders(), signal: controller.signal });
  if (!health.ok) throw new Error(`health returned HTTP ${health.status}`);
  const rooms = await fetch(endpoint('/api/rooms'), { headers: authHeaders(), signal: controller.signal });
  if (!rooms.ok || !Array.isArray(await rooms.json())) throw new Error(`rooms list failed: HTTP ${rooms.status}`);

  const name = `synthetic-${Date.now().toString(36)}`;
  const create = await fetch(endpoint('/api/rooms/create'), {
    method: 'POST', headers: { 'content-type': 'application/json', ...authHeaders() },
    body: JSON.stringify({ name }), signal: controller.signal,
  });
  if (!create.ok) throw new Error(`room creation failed: HTTP ${create.status}`);
  const room = await create.json();
  if (!room.id) throw new Error('room creation returned no room id');

  const join = await fetch(endpoint('/api/rooms/join'), {
    method: 'POST', headers: { 'content-type': 'application/json', ...authHeaders() },
    body: JSON.stringify({ room_id: room.id }), signal: controller.signal,
  });
  if (!join.ok) throw new Error(`room join failed: HTTP ${join.status}`);
  const joinBody = await join.json();
  if (joinBody.error) throw new Error(`room join returned an error: ${joinBody.error}`);

  const wsProtocol = base.protocol === 'https:' ? 'wss:' : 'ws:';
  const wsURL = `${wsProtocol}//${base.host}/rooms/${encodeURIComponent(room.id)}/ws`;
  const players = [openPlayer(wsURL), openPlayer(wsURL)];
  try {
    const joined = await Promise.all(players.map((player) => player.joined));
    const assignments = new Set(joined.map((message) => message.player));
    if (assignments.size !== 2 || !assignments.has(1) || !assignments.has(2)) {
      throw new Error(`expected unique player assignments 1 and 2, got ${[...assignments].join(',')}`);
    }
    await Promise.all(players.map((player) => player.state));
    console.log(`synthetic passed: room ${room.id}, two players joined and received state`);
  } finally {
    for (const player of players) player.socket.close();
  }
} finally {
  clearTimeout(abortTimer);
}

function openPlayer(url) {
  const socket = new WebSocket(url, { headers: { ...authHeaders(), Origin: origin } });
  const joined = waitFor(socket, (message) => message.type === 'joined', 'player assignment');
  const state = waitFor(socket, (message) => message.type === 'state', 'player state');
  socket.on('open', () => socket.send(JSON.stringify({ type: 'proxy-ready' })));
  return { socket, joined, state };
}

function waitFor(socket, predicate, description) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      socket.close();
      reject(new Error(`timed out waiting for ${description}`));
    }, timeoutMs);
    socket.on('message', (payload) => {
      try {
        const message = JSON.parse(payload.toString());
        if (message.type === 'error') throw new Error(message.message || `${description} failed`);
        if (predicate(message)) {
          clearTimeout(timer);
          resolve(message);
        }
      } catch (error) {
        clearTimeout(timer);
        reject(error);
      }
    });
    socket.on('error', (error) => {
      clearTimeout(timer);
      reject(new Error(`${description}: ${error.message}`));
    });
  });
}
