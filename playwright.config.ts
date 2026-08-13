import { defineConfig, devices } from '@playwright/test';
import { isIP } from 'node:net';

const isCI = !!process.env.CI;
const isK8s = process.env.TEST_MODE === 'k8s';
const baseURL = process.env.BASE_URL || 'http://localhost:8080';
const base = new URL(baseURL);
const hostname = base.hostname.toLowerCase().replace(/^\[|\]$/gu, '').replace(/\.$/u, '');
const localTarget = hostname === 'localhost' || hostname.endsWith('.localhost')
  || (isIP(hostname) === 4 && hostname.startsWith('127.'))
  || (isIP(hostname) === 6 && (hostname === '::1' || hostname.startsWith('::ffff:127.')));
const canonicalTarget = hostname === 'pong.belacca.com';
if (canonicalTarget) throw new Error('Playwright experiment target rejected: canonical public Pong production is never a test target');
if (!localTarget && (process.env.PONG_EXPERIMENT_MODE !== 'capacity' && process.env.PONG_EXPERIMENT_MODE !== 'chaos'
  || process.env.PONG_EXPERIMENT_APPROVED !== '1'
  || process.env.PONG_EXPERIMENT_TARGET !== 'isolated')) {
  throw new Error('Playwright target rejected: non-local tests require approved isolated experiment configuration');
}

export default defineConfig({
  testDir: './tests',
  timeout: 30000,
  expect: { timeout: 5000 },
  fullyParallel: false,
  forbidOnly: isCI,
  // Browser/WebSocket load must never fan out implicitly. Experiments must
  // opt into the isolated target guard above, but remain one worker/no retry.
  retries: 0,
  workers: 1,
  reporter: isCI ? [
    ['list'],
    ['html', { outputFolder: 'playwright-report', open: 'never' }],
  ] : 'html',
  use: {
    baseURL,
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      testIgnore: /mobile\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'mobile-pixel-7',
      testMatch: /mobile\.spec\.ts/,
      use: { ...devices['Pixel 7'] },
    },
    {
      name: 'mobile-iphone-13-webkit',
      testMatch: /mobile\.spec\.ts/,
      use: { ...devices['iPhone 13'] },
    },
  ],
  // K8s tests use the already-running gateway. Local tests run the current
  // source through `go run` so an ignored stale binary cannot mask changes.
  webServer: isK8s ? undefined : {
    command: 'go run . --mode=local',
    port: 8080,
    reuseExistingServer: !isCI,
  },
});