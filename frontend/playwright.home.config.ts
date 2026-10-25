import { defineConfig } from "@playwright/test";

// The website's test server listens on 8092 unless HOME_TEST_PORT says
// otherwise (another program may hold that port on a developer machine).
const port = process.env.HOME_TEST_PORT ?? "8092";

export default defineConfig({
  testDir: "./home/tests",
  fullyParallel: true,
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: "retain-on-failure",
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE }
      : {},
  },
  webServer: {
    command: `bun run build:home && cd .. && go run ./cmd/paymux-home -listen 127.0.0.1:${port}`,
    url: `http://127.0.0.1:${port}/healthz`,
    timeout: 60_000,
  },
});
