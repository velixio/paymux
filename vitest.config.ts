import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";

export default defineConfig({
  resolve: {
    alias: { "@": fileURLToPath(new URL("./frontend", import.meta.url)) },
  },
  test: {
    include: [
      "web-home/src/**/*.spec.{ts,tsx}",
      "web-dashboard/src/**/*.test.ts",
    ],
    environment: "happy-dom",
    coverage: {
      provider: "v8",
      thresholds: {
        statements: 100,
        branches: 100,
        functions: 100,
        lines: 100,
      },
      reporter: ["text", "json-summary", "html", "lcov"],
      reportsDirectory: "coverage/frontend",
      include: [
        "frontend/**/*.{ts,tsx}",
        "web-dashboard/src/**/*.{ts,tsx}",
        "web-home/src/**/*.{ts,tsx}",
      ],
      exclude: ["**/*.test.*", "**/*.spec.*"],
    },
  },
});
