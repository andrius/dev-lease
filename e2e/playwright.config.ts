import { defineConfig, devices } from '@playwright/test';

// The dashboard under test. The specs run from this directory; the serve
// setup and teardown live in the CI workflow and in e2e/README.md.
export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  // One worker and no parallelism on purpose: the specs share one serve
  // and one pair of demo stacks, and the flows change their state. The
  // file order is the run order, so the matrix pages run before the flows.
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  reporter: [['list']],
  // Playwright's own artifacts (failure screenshots, traces) land in the
  // same place as the spec screenshots, gitignored either way.
  // Playwright cleans outputDir at the start of a run, so it never
  // shares a directory with the screenshots.
  outputDir: 'test-results',
  timeout: 120_000,
  expect: { timeout: 15_000 },
  use: {
    baseURL: process.env.DEV_LEASE_E2E_URL ?? 'http://127.0.0.1:8410',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
