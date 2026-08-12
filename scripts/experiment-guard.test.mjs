import assert from 'node:assert/strict';
import test from 'node:test';
import { parseBoundedInteger, parseExperimentURL, validateExperimentTarget } from './experiment-guard.mjs';

test('canonical public hostname and documented public edges are always denied', () => {
  for (const value of ['https://pong.belacca.com', 'http://169.58.97.73', 'http://169.58.97.41', 'http://169.58.97.42']) {
    const result = validateExperimentTarget(parseExperimentURL(value), {
      PONG_EXPERIMENT_MODE: 'capacity', PONG_EXPERIMENT_APPROVED: '1', PONG_EXPERIMENT_TARGET: 'isolated',
    });
    assert.equal(result.ok, false, value);
    assert.equal(result.code, 'canonical_public_target');
  }
});

test('non-local target requires mode, approval, and isolated marker', () => {
  const target = parseExperimentURL('https://dev.example.invalid');
  assert.equal(validateExperimentTarget(target, {}).ok, false);
  assert.equal(validateExperimentTarget(target, { PONG_EXPERIMENT_MODE: 'capacity' }).code, 'experiment_approval_required');
  assert.equal(validateExperimentTarget(target, { PONG_EXPERIMENT_MODE: 'capacity', PONG_EXPERIMENT_APPROVED: '1' }).code, 'isolated_target_required');
  assert.deepEqual(validateExperimentTarget(target, {
    PONG_EXPERIMENT_MODE: 'chaos', PONG_EXPERIMENT_APPROVED: '1', PONG_EXPERIMENT_TARGET: 'isolated',
  }), { ok: true, local: false, mode: 'chaos' });
});

test('loopback targets are allowed without experiment approval', () => {
  for (const value of ['http://localhost:8080', 'http://127.0.0.1:8080', 'http://[::1]:8080', 'http://dev.localhost:8080']) {
    assert.deepEqual(validateExperimentTarget(parseExperimentURL(value), {}), { ok: true, local: true });
  }
});

test('bounded integer parser rejects malformed and out-of-range values', () => {
  assert.equal(parseBoundedInteger('', 3, 1, 50, 'iterations'), 3);
  assert.equal(parseBoundedInteger('03', 3, 1, 50, 'iterations'), 3);
  for (const value of ['1.5', '-1', 'abc', '0', '51']) {
    assert.throws(() => parseBoundedInteger(value, 3, 1, 50, 'iterations'), /iterations/u);
  }
});
