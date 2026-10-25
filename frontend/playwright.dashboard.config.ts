import { defineConfig } from "@playwright/test";

// The hub is started (against a seeded throw-away data directory) by
// globalSetup, because seeding must finish before the server takes the SQLite
// lock; so there is no webServer entry here.
export const port = Number(process.env.PAYMUX_E2E_PORT ?? 8093);

export default defineConfig({
  testDir: "./dashboard/e2e",
  testMatch: "*.spec.ts",
  globalSetup: "./dashboard/e2e/global-setup.ts",
  fullyParallel: false,
  workers: 1,
  timeout: 60_000,
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: "retain-on-failure",
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE }
      : {},
  },
});
