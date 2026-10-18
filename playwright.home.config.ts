import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./web-home/tests",
  fullyParallel: true,
  use: {
    baseURL: "http://127.0.0.1:8092",
    trace: "retain-on-failure",
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE }
      : {},
  },
  webServer: {
    command: "bun run build:home && go run ./web-home -listen 127.0.0.1:8092",
    url: "http://127.0.0.1:8092/healthz",
    timeout: 60_000,
  },
});
