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
      PONG_EXPERIMENT_MODE: '',
      PONG_EXPERIMENT_APPROVED: '',
      PONG_EXPERIMENT_TARGET: '',
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
      PONG_EXPERIMENT_MODE: '',
      PONG_EXPERIMENT_APPROVED: '',
      PONG_EXPERIMENT_TARGET: '',
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

test('capacity and chaos workflows share one global lock and explicit approval', async () => {
  const capacity = await readFile(new URL('../.github/workflows/capacity-experiment.yml', import.meta.url), 'utf8');
  const chaos = await readFile(new URL('../.github/workflows/chaos-experiment.yml', import.meta.url), 'utf8');
  assert.match(capacity, /group: capacity-chaos-experiment/u);
  assert.match(chaos, /group: capacity-chaos-experiment/u);
  assert.match(capacity, /approve_experiment:/u);
  assert.match(chaos, /approve_experiment:/u);
  assert.doesNotMatch(capacity + chaos, /matrix:/u);
  assert.doesNotMatch(capacity + chaos, /retries:/u);
});

test('capacity workflow keeps its disposable cluster name within k3d limits', async () => {
  const workflow = await readFile(new URL('../.github/workflows/capacity-experiment.yml', import.meta.url), 'utf8');
  assert.match(workflow, /CLUSTER_NAME: cnp-capacity-\$\{\{ github\.run_id \}\}/u);
  assert.match(workflow, /KUBE_CONTEXT: k3d-cnp-capacity-\$\{\{ github\.run_id \}\}/u);
  assert.doesNotMatch(workflow, /cloudnativepong-capacity-/u);
  assert.match(workflow, /pong-metrics\.txt/u);
  assert.match(workflow, /cleanup-verification/u);
  assert.match(workflow, /abort_threshold/u);
});

test('chaos workflow is one-fault-at-a-time with bounded comparable drills', async () => {
  const workflow = await readFile(new URL('../.github/workflows/chaos-experiment.yml', import.meta.url), 'utf8');
  for (const scenario of ['api-restart', 'gateway-restart', 'room-termination', 'node-drain', 'resource-pressure']) {
    assert.match(workflow, new RegExp(scenario, 'u'));
  }
  assert.match(workflow, /RUNS: '3'/u);
  assert.match(workflow, /RECOVERY_TARGET_MS: '360000'/u);
  assert.match(workflow, /namespace-cleanup/u);
  assert.match(workflow, /cluster-cleanup/u);
  assert.doesNotMatch(workflow, /strategy:\s*\n\s+matrix:/u);
});

test('Playwright remains one worker with no retries and rejects public targets', async () => {
  const config = await readFile(new URL('../playwright.config.ts', import.meta.url), 'utf8');
  assert.match(config, /workers: 1/u);
  assert.match(config, /retries: 0/u);
  assert.match(config, /canonical public Pong production/u);
  assert.match(config, /PONG_EXPERIMENT_TARGET/u);
});

test('dry-run defaults to a local target and emits aggregate metadata only', () => {
  const result = run(['--dry-run']);

  assert.equal(result.status, 0);
  assert.doesNotMatch(combinedOutput(result), /https?:\/\//u);
  assert.deepEqual(JSON.parse(result.stdout), {
    mode: 'dry-run',
    operations: ['health', 'create', 'join', 'websocket', 'cleanup'],
    limits: {
      iterations: 3,
      concurrency: 1,
      timeout_ms: 10_000,
      max_duration_ms: 60_000,
      abort_threshold: 3,
    },
    output: 'aggregate counts, throughput, recovery, and latency percentiles only',
  });
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
  assert.match(combinedOutput(result), /never an experiment target/u);
});

test('every other non-local target is rejected without explicit authorization', () => {
  const result = run(['--dry-run', '--base-url=https://disposable.example.invalid']);

  assert.equal(result.status, 2);
  assert.match(combinedOutput(result), /non-local target/u);
});

test('an exact experiment authorization permits a non-local dry-run without exposing its target', () => {
  const result = run(
    ['--dry-run', '--base-url=https://disposable.example.invalid'],
    {
      PONG_EXPERIMENT_MODE: 'capacity',
      PONG_EXPERIMENT_APPROVED: '1',
      PONG_EXPERIMENT_TARGET: 'isolated',
    },
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
  assert.match(combinedOutput(result), /approved experiment mode/u);
});

test('malformed, fractional, and out-of-range numeric inputs fail closed', () => {
  for (const flag of ['--iterations=1.5', '--concurrency=0', '--timeout-ms=30001', '--max-duration-ms=180001', '--abort-threshold=21']) {
    const result = run(['--dry-run', flag]);
    assert.equal(result.status, 2, flag);
    assert.match(combinedOutput(result), /configuration rejected/u);
  }
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
