import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";

export default defineConfig({
  resolve: {
    alias: { "@": fileURLToPath(new URL(".", import.meta.url)) },
  },
  // The website renders the repository's Markdown guides (docs/*.md).
  server: { fs: { allow: [fileURLToPath(new URL("..", import.meta.url))] } },
  test: {
    include: [
      "home/src/**/*.spec.{ts,tsx}",
      "dashboard/src/**/*.test.ts",
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
      reportsDirectory: "../coverage/frontend",
      include: [
        "components/**/*.{ts,tsx}",
        "lib/**/*.{ts,tsx}",
        "dashboard/src/**/*.{ts,tsx}",
        "home/src/**/*.{ts,tsx}",
      ],
      exclude: ["**/*.test.*", "**/*.spec.*"],
    },
  },
});
