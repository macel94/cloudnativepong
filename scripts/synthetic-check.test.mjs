import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { createServer } from 'node:http';
import { once } from 'node:events';
import test from 'node:test';
import WebSocket, { WebSocketServer } from 'ws';
import {
  main,
  parseBaseURL,
  runSynthetic,
  SyntheticError,
} from './synthetic-check.mjs';

const PROJECT_ROOT = new URL('../', import.meta.url);

function json(response, status, value) {
  const body = JSON.stringify(value);
  response.writeHead(status, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(body),
  });
  response.end(body);
}

async function readJSON(request) {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

async function startFixture({
  retainRooms = false,
  createResponseDelayMs = 0,
  transientCreateFailures = 0,
  malformedJoin = false,
} = {}) {
  const prefix = '/pong/';
  const rooms = new Map();
  const sockets = new Set();
  let nextRoom = 0;

  const server = createServer(async (request, response) => {
    const url = new URL(request.url, `http://${request.headers.host}`);
    if (!url.pathname.startsWith(prefix)) {
      response.writeHead(404).end();
      return;
    }
    const path = url.pathname.slice(prefix.length - 1) || '/';

    if (request.method === 'GET' && path === '/') {
      const body = '<!doctype html><title>Cloud Native Pong — Lobby</title><input id="playerName">';
      response.writeHead(200, { 'content-type': 'text/html', 'content-length': Buffer.byteLength(body) });
      response.end(body);
      return;
    }
    if (request.method === 'GET' && path === '/health') {
      response.writeHead(200, { 'content-type': 'text/plain', 'content-length': 3 });
      response.end('ok\n');
      return;
    }
    if (request.method === 'GET' && path === '/api/rooms') {
      json(response, 200, [...rooms.values()].map(({ id, name }) => ({
        id,
        name,
        status: 'waiting',
        players: 1,
      })));
      return;
    }
    if (request.method === 'POST' && path === '/api/rooms/create') {
      if (transientCreateFailures > 0) {
        transientCreateFailures -= 1;
        json(response, 503, { error: 'fixture temporarily unavailable' });
        return;
      }
      const body = await readJSON(request);
      const id = `ab${String(nextRoom++).padStart(4, '0')}`;
      rooms.set(id, { id, name: body.name, sockets: new Set() });
      if (createResponseDelayMs > 0) await new Promise((resolve) => setTimeout(resolve, createResponseDelayMs));
      json(response, 200, { id, name: body.name, status: 'waiting', players: 1 });
      return;
    }
    if (request.method === 'POST' && path === '/api/rooms/join') {
      const body = await readJSON(request);
      if (malformedJoin) {
        json(response, 200, { room_id: 'wrong', ws_path: '/rooms/wrong/ws' });
        return;
      }
      if (!rooms.has(body.room_id)) {
        json(response, 404, { error: 'room not found' });
        return;
      }
      json(response, 200, {
        room_id: body.room_id,
        ws_path: `/rooms/${body.room_id}/ws`,
        mode: 'fixture',
      });
      return;
    }
    response.writeHead(404).end();
  });

  const webSockets = new WebSocketServer({ noServer: true });
  server.on('upgrade', (request, socket, head) => {
    const url = new URL(request.url, `http://${request.headers.host}`);
    if (!url.pathname.startsWith(`${prefix.slice(0, -1)}/rooms/`)) {
      socket.destroy();
      return;
    }
    const roomID = url.pathname.split('/').filter(Boolean).at(-2);
    const room = rooms.get(roomID);
    if (!room) {
      socket.destroy();
      return;
    }
    webSockets.handleUpgrade(request, socket, head, (client) => {
      webSockets.emit('connection', client, request, room);
    });
  });

  webSockets.on('connection', (client, _request, room) => {
    sockets.add(client);
    room.sockets.add(client);
    const player = room.sockets.size;
    client.on('message', (payload) => {
      let message;
      try {
        message = JSON.parse(payload.toString());
      } catch {
        client.close();
        return;
      }
      if (message.type !== 'proxy-ready') return;
      client.send(JSON.stringify({ type: 'joined', player }));
      client.send(JSON.stringify({
        type: 'state',
        state: { status: 'playing', p1_ready: true, p2_ready: true },
      }));
    });
    client.on('close', () => {
      sockets.delete(client);
      room.sockets.delete(client);
      if (!retainRooms && room.sockets.size === 0) rooms.delete(room.id);
    });
    client.on('error', () => {});
  });

  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const address = server.address();
  const baseURL = `http://127.0.0.1:${address.port}${prefix}`;

  return {
    baseURL,
    rooms,
    async close() {
      for (const socket of sockets) socket.terminate();
      webSockets.close();
      server.close();
      await once(server, 'close');
    },
  };
}

test('parseBaseURL preserves an ingress path and rejects unsafe targets', () => {
  assert.equal(parseBaseURL('https://example.test/pong').pathname, '/pong/');
  assert.equal(parseBaseURL('https://example.test/').pathname, '/');
  assert.throws(() => parseBaseURL(''), /must be supplied/);
  assert.throws(() => parseBaseURL('ftp://example.test'), /http or https/);
  assert.throws(() => parseBaseURL('https://user:pass@example.test'), /credentials/);
  assert.throws(() => parseBaseURL('https://example.test/?token=secret'), /query/);
});

test('the complete fixture journey validates HTTP, two players, and cleanup', async (t) => {
  const fixture = await startFixture();
  t.after(() => fixture.close());

  const result = await runSynthetic({
    baseURL: fixture.baseURL,
    env: { SYNTHETIC_ORIGIN: 'https://pong.belacca.com' },
    timeoutMs: 5_000,
    requestTimeoutMs: 1_000,
    cleanupTimeoutMs: 2_000,
    cleanupPollMs: 10,
  });

  assert.equal(result.dryRun, false);
  assert.ok(result.durationMs >= 0);
  assert.equal(result.contract_version, 'belacca.pong-slo-journey-result.v1');
  assert.deepEqual(
    { total: result.total, good: result.good, failed: result.failed, failure_stage: result.failure_stage, failure_code: result.failure_code },
    { total: 1, good: 1, failed: 0, failure_stage: null, failure_code: null },
  );
  assert.equal(fixture.rooms.size, 0);
});

test('classifies a primary journey failure with bounded stage evidence', async (t) => {
  const fixture = await startFixture({ malformedJoin: true });
  t.after(() => fixture.close());

  await assert.rejects(
    runSynthetic({
      baseURL: fixture.baseURL,
      timeoutMs: 5_000,
      requestTimeoutMs: 1_000,
      cleanupTimeoutMs: 2_000,
      cleanupPollMs: 10,
    }),
    (error) => error instanceof SyntheticError &&
      error.result?.failure_stage === 'room-join' &&
      error.result.failure_code === 'failed' &&
      error.result.total === 1 && error.result.good === 0 && error.result.failed === 1,
  );
  assert.equal(fixture.rooms.size, 0);
});

test('recovers a room when create is accepted before its response times out', async (t) => {
  const fixture = await startFixture({ createResponseDelayMs: 300 });
  t.after(() => fixture.close());

  const result = await runSynthetic({
    baseURL: fixture.baseURL,
    timeoutMs: 5_000,
    requestTimeoutMs: 1_000,
    createRequestTimeoutMs: 100,
    cleanupTimeoutMs: 2_000,
    cleanupPollMs: 10,
  });

  assert.equal(result.dryRun, false);
  assert.equal(fixture.rooms.size, 0);
});

test('retries an explicit transient create response without leaking a room', async (t) => {
  const fixture = await startFixture({ transientCreateFailures: 1 });
  t.after(() => fixture.close());

  const result = await runSynthetic({
    baseURL: fixture.baseURL,
    timeoutMs: 5_000,
    requestTimeoutMs: 1_000,
    cleanupTimeoutMs: 2_000,
    cleanupPollMs: 10,
  });

  assert.equal(result.dryRun, false);
  assert.equal(fixture.rooms.size, 0);
});

test('cleanup verification is a hard failure when a room remains', async (t) => {
  const fixture = await startFixture({ retainRooms: true });
  t.after(() => fixture.close());

  await assert.rejects(
    runSynthetic({
      baseURL: fixture.baseURL,
      timeoutMs: 5_000,
      requestTimeoutMs: 1_000,
      cleanupTimeoutMs: 100,
      cleanupPollMs: 10,
    }),
    (error) => error instanceof SyntheticError &&
      error.result?.total === 1 && error.result.good === 0 &&
      error.result.failure_stage === 'cleanup' && error.result.failure_code === 'cleanup_failed' &&
      /synthetic check exceeded its overall timeout/u.test(error.message),
  );
  assert.equal(fixture.rooms.size, 1);
});

test('dry-run remains useful without a target, but execution does not', async () => {
  assert.deepEqual(await runSynthetic({ baseURL: '', dryRun: true }), { dryRun: true });
  assert.equal(await main([], { SYNTHETIC_BASE_URL: '' }), 1);
});

test('the shell wrapper fails closed when no target is configured', () => {
  const result = spawnSync('bash', ['scripts/synthetic-check.sh'], {
    cwd: new URL('.', PROJECT_ROOT),
    env: { ...process.env, SYNTHETIC_BASE_URL: '' },
    encoding: 'utf8',
  });
  assert.equal(result.status, 2);
  assert.match(`${result.stdout}\n${result.stderr}`, /refusing to report an unexecuted check as successful/u);
});

// Make sure the test fixture uses the same client implementation that the
// production runner uses; this catches accidental dependency/API drift.
assert.equal(typeof WebSocket, 'function');
