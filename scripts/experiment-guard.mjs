import { isIP } from 'node:net';

export const CANONICAL_PUBLIC_HOSTNAMES = new Set([
  'pong.belacca.com',
  // Documented native public edges; never use them as experiment targets.
  '169.58.97.73',
  '169.58.97.41',
  '169.58.97.42',
]);
export const EXPERIMENT_APPROVAL_VALUE = '1';
export const ISOLATED_TARGET_VALUE = 'isolated';
export const EXPERIMENT_MODES = new Set(['capacity', 'chaos']);

export function normalizeHostname(hostname) {
  return hostname.toLowerCase().replace(/^\[|\]$/gu, '').replace(/\.$/u, '');
}

export function isLocalTarget(url) {
  const hostname = normalizeHostname(url.hostname);
  if (hostname === 'localhost' || hostname.endsWith('.localhost')) return true;
  if (isIP(hostname) === 4) return hostname.startsWith('127.');
  if (isIP(hostname) === 6) return hostname === '::1' || hostname.startsWith('::ffff:127.');
  return false;
}

export function validateExperimentTarget(base, env = process.env) {
  const hostname = normalizeHostname(base.hostname);
  if (CANONICAL_PUBLIC_HOSTNAMES.has(hostname)) {
    return { ok: false, code: 'canonical_public_target', message: 'canonical public Pong production is never an experiment target' };
  }
  if (isLocalTarget(base)) return { ok: true, local: true };

  const mode = env.PONG_EXPERIMENT_MODE || '';
  if (!EXPERIMENT_MODES.has(mode)) {
    return { ok: false, code: 'experiment_mode_required', message: 'non-local targets require an approved experiment mode' };
  }
  if (env.PONG_EXPERIMENT_APPROVED !== EXPERIMENT_APPROVAL_VALUE) {
    return { ok: false, code: 'experiment_approval_required', message: 'non-local targets require explicit experiment approval' };
  }
  if (env.PONG_EXPERIMENT_TARGET !== ISOLATED_TARGET_VALUE) {
    return { ok: false, code: 'isolated_target_required', message: 'non-local targets require the isolated target marker' };
  }
  return { ok: true, local: false, mode };
}

export function parseBoundedInteger(value, fallback, minimum, maximum, name) {
  if (value === undefined || value === '') return fallback;
  if (!/^[0-9]+$/u.test(String(value))) {
    throw new Error(`${name} must be a decimal integer between ${minimum} and ${maximum}`);
  }
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < minimum || parsed > maximum) {
    throw new Error(`${name} must be between ${minimum} and ${maximum}`);
  }
  return parsed;
}

export function parseExperimentURL(value) {
  let base;
  try {
    base = new URL(value);
  } catch {
    throw new Error('base URL must use http or https');
  }
  if (!['http:', 'https:'].includes(base.protocol)) throw new Error('base URL must use http or https');
  if (base.username || base.password || base.search || base.hash) {
    throw new Error('base URL must not contain credentials, a query, or a fragment');
  }
  return base;
}
