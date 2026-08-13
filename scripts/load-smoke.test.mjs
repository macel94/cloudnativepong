import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { createServer } from 'node:http';
import { once } from 'node:events';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { readFile } from 'node:fs/promises';

const PROJECT_ROOT = fileURLToPath(new URL('../', import.meta.url));
const SCRIPT = fileURLToPath(new URL('./load-smoke.mjs', import.meta.url));

function run(args = [], environment = {}) {
  return spawnSync(process.execPath, [SCRIPT, ...args], {
    cwd: PROJECT_ROOT,
    env: {
      ...process.env,
      LOAD_SMOKE_BASE_URL: '',
      LOAD_SMOKE_EXPERIMENT_APPROVED: '',
      ...environment,
    },
    encoding: 'utf8',
  });
}

function combinedOutput(result) {
  return `${result.stdout}\n${result.stderr}`;
}

async function runAsync(args = [], environment = {}) {
  const child = spawn(process.execPath, [SCRIPT, ...args], {
    cwd: PROJECT_ROOT,
    env: {
      ...process.env,
      LOAD_SMOKE_BASE_URL: '',
      LOAD_SMOKE_EXPERIMENT_APPROVED: '',
      ...environment,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const stdout = [];
  const stderr = [];
  child.stdout.on('data', (chunk) => stdout.push(chunk));
  child.stderr.on('data', (chunk) => stderr.push(chunk));
  const [code, signal] = await once(child, 'close');
  return {
    status: code,
    signal,
    stdout: Buffer.concat(stdout).toString(),
    stderr: Buffer.concat(stderr).toString(),
  };
}

test('capacity policy matches the configured topology and safe headroom model', async () => {
  const policy = JSON.parse(await readFile(new URL('../capacity-policy.json', import.meta.url), 'utf8'));
  assert.equal(policy.topology.api_replicas, 1);
  assert.equal(policy.topology.sqlite_writers, 1);
  assert.equal(policy.admission.global_websocket_sessions, 128);
  assert.equal(policy.capacity_model.safe_global_websocket_sessions, 102);
  assert.equal(policy.capacity_model.safe_two_player_games_if_no_spectators, 51);
  assert.equal(policy.capacity_model.room_quota_review_threshold, 92);
  assert.equal(policy.topology.room_resource_quota.dynamic_room_pod_ceiling, 115);
  assert.equal(policy.overload_policy.status, 429);
  assert.equal(policy.overload_policy.retry_after_seconds, 60);
  assert.equal(policy.scaling_boundary.not_implemented, true);
});

test('capacity workflow keeps its disposable cluster name within k3d limits', async () => {
  const workflow = await readFile(new URL('../.github/workflows/capacity-experiment.yml', import.meta.url), 'utf8');
  assert.match(workflow, /CLUSTER_NAME: cnp-capacity-\$\{\{ github\.run_id \}\}/u);
  assert.match(workflow, /KUBE_CONTEXT: k3d-cnp-capacity-\$\{\{ github\.run_id \}\}/u);
  assert.doesNotMatch(workflow, /cloudnativepong-capacity-/u);
  assert.match(workflow, /pong-metrics\.txt/u);
  assert.match(workflow, /LOAD_SMOKE_ITERATIONS.*50|ITERATIONS.*50/u);
  assert.match(workflow, /LOAD_SMOKE_EXPERIMENT_APPROVED/u);
});

test('dry-run defaults to a local target and emits aggregate metadata only', () => {
  const result = run(['--dry-run']);

  assert.equal(result.status, 0);
  assert.doesNotMatch(combinedOutput(result), /https?:\/\//u);
  assert.deepEqual(JSON.parse(result.stdout), {
    mode: 'dry-run',
    operations: ['health', 'create', 'join', 'websocket', 'api_read', 'cleanup'],
    limits: {
      iterations: 3,
      concurrency: 1,
      timeout_ms: 10_000,
      max_duration_ms: 60_000,
    },
    output: 'aggregate counts and latency percentiles only',
  });
});

test('invalid benchmark bounds fail closed instead of being clamped', () => {
  for (const flag of ['iterations=0', 'concurrency=9', 'timeout-ms=499', 'max-duration-ms=180001', 'iterations=1.5']) {
    const result = run(['--dry-run', `--${flag}`]);
    assert.equal(result.status, 2, flag);
    assert.match(combinedOutput(result), /configuration rejected/u);
  }
});

test('loopback targets remain usable without experiment authorization', () => {
  const result = run(['--dry-run', '--base-url=http://127.0.0.1:8080']);

  assert.equal(result.status, 0);
  assert.doesNotMatch(combinedOutput(result), /LOAD_SMOKE_EXPERIMENT_APPROVED/u);
});

test('canonical public production is rejected without explicit authorization', () => {
  const result = run(['--dry-run', '--base-url=https://pong.belacca.com']);

  assert.equal(result.status, 2);
  assert.match(combinedOutput(result), /canonical public Pong production/u);
  assert.match(combinedOutput(result), /LOAD_SMOKE_EXPERIMENT_APPROVED=1/u);
});

test('every other non-local target is rejected without explicit authorization', () => {
  const result = run(['--dry-run', '--base-url=https://disposable.example.invalid']);

  assert.equal(result.status, 2);
  assert.match(combinedOutput(result), /non-local target/u);
});

test('an exact experiment authorization permits a non-local dry-run without exposing its target', () => {
  const result = run(
    ['--dry-run', '--base-url=https://disposable.example.invalid'],
    { LOAD_SMOKE_EXPERIMENT_APPROVED: '1' },
  );

  assert.equal(result.status, 0);
  assert.doesNotMatch(combinedOutput(result), /disposable\.example\.invalid/u);
  assert.doesNotMatch(combinedOutput(result), /https?:\/\//u);
});

test('truthy but non-explicit authorization values are rejected', () => {
  const result = run(
    ['--dry-run', '--base-url=https://disposable.example.invalid'],
    { LOAD_SMOKE_EXPERIMENT_APPROVED: 'true' },
  );

  assert.equal(result.status, 2);
  assert.match(combinedOutput(result), /LOAD_SMOKE_EXPERIMENT_APPROVED=1/u);
});

test('a malformed join response fails the journey contract', async (t) => {
  const server = createServer((request, response) => {
    const url = new URL(request.url, 'http://127.0.0.1');
    if (url.pathname === '/health') {
      response.writeHead(200, { 'content-type': 'text/plain' });
      response.end('ok\\n');
      return;
    }
    if (url.pathname === '/api/rooms/create') {
      response.writeHead(200, { 'content-type': 'application/json' });
      response.end(JSON.stringify({ id: 'abcdef', status: 'waiting' }));
      return;
    }
    if (url.pathname === '/api/rooms/join') {
      response.writeHead(200, { 'content-type': 'application/json' });
      response.end(JSON.stringify({ room_id: 'wrong', ws_path: '/rooms/wrong/ws' }));
      return;
    }
    if (url.pathname === '/api/rooms') {
      response.writeHead(200, { 'content-type': 'application/json' });
      response.end('[]');
      return;
    }
    response.writeHead(404).end();
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => server.close());
  const { port } = server.address();

  const result = await runAsync([
    `--base-url=http://127.0.0.1:${port}`,
    '--iterations=1',
    '--concurrency=1',
    '--timeout-ms=1000',
    '--max-duration-ms=5000',
  ]);

  assert.equal(result.status, 1);
  assert.match(result.stdout, /join_contract/u);
});

test('overload evidence records HTTP statuses and Retry-After headers', async (t) => {
  const server = createServer((request, response) => {
    const url = new URL(request.url, 'http://127.0.0.1');
    if (url.pathname === '/health') {
      response.writeHead(200, { 'content-type': 'text/plain' });
      response.end('ok\n');
      return;
    }
    response.writeHead(429, { 'content-type': 'application/json', 'retry-after': '60' });
    response.end(JSON.stringify({ error: 'too many requests' }));
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => server.close());
  const { port } = server.address();

  const result = await runAsync([
    `--base-url=http://127.0.0.1:${port}`,
    '--iterations=1',
    '--concurrency=1',
    '--timeout-ms=1000',
    '--max-duration-ms=5000',
  ]);
  assert.equal(result.status, 1);
  const output = JSON.parse(result.stdout);
  assert.equal(output.http_statuses['429'], 1);
  assert.equal(output.retry_after_responses, 1);
  assert.equal(output.operations.create.status_codes['429'], 1);
  assert.equal(output.failure_codes.create_http_4xx, 1);
});

test('request bodies cannot extend the overall experiment deadline', async (t) => {
  const server = createServer((request, response) => {
    const url = new URL(request.url, 'http://127.0.0.1');
    if (url.pathname === '/health') {
      response.writeHead(200, { 'content-type': 'text/plain' });
      response.write('ok');
      return;
    }
    response.writeHead(404).end();
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => server.close());
  const { port } = server.address();
  const started = Date.now();

  const result = await runAsync([
    `--base-url=http://127.0.0.1:${port}`,
    '--iterations=1',
    '--concurrency=1',
    '--timeout-ms=5000',
    '--max-duration-ms=1000',
  ]);

  assert.equal(result.status, 1);
  assert.ok(Date.now() - started < 3000, 'request exceeded the bounded experiment deadline');
  assert.match(result.stdout, /health_deadline/u);
});
