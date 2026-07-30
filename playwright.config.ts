import { defineConfig, devices } from '@playwright/test';

const isCI = !!process.env.CI;
const isK8s = process.env.TEST_MODE === 'k8s';
const baseURL = process.env.BASE_URL || 'http://localhost:8080';

export default defineConfig({
  testDir: './tests',
  timeout: 30000,
  expect: { timeout: 5000 },
  fullyParallel: true,
  forbidOnly: isCI,
  retries: isCI ? 2 : 0,
  workers: isCI ? 1 : undefined,
  reporter: isCI ? 'list' : 'html',
  use: {
    baseURL,
    trace: 'on-first-retry',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  // K8s tests use the already-running gateway; only local tests start a binary.
  webServer: isK8s ? undefined : {
    command: './cloudnativepong --mode=local',
    port: 8080,
    reuseExistingServer: !isCI,
  },
});