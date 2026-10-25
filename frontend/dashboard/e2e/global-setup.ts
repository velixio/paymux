import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const OWNER_EMAIL = "owner@example.com";
export const OWNER_PASSWORD = "correct horse battery";

const root = resolve(fileURLToPath(new URL("../../..", import.meta.url)));
const port = Number(process.env.PAYMUX_E2E_PORT ?? 8093);

// Builds the hub, seeds a throw-away data directory and serves it on
// 127.0.0.1:<port> with 2FA skipped (development mode only). The seed uses the
// CLI for the owner, app and FX rate, and cmd/paymux-e2eseed (a separate
// binary, unreachable from the production one) for payments and ledger rows.
export default async function globalSetup(): Promise<() => Promise<void>> {
  const dataDir = mkdtempSync(join(tmpdir(), "paymux-e2e-"));
  const bin = join(dataDir, "paymux");
  const seedBin = join(dataDir, "paymux-e2eseed");
  const build = {
    cwd: root,
    stdio: "inherit" as const,
    env: { ...process.env, CGO_ENABLED: "0" },
  };
  if (!process.env.PAYMUX_E2E_SKIP_FRONTEND_BUILD) {
    execFileSync("bun", ["run", "--cwd", "frontend", "build:dashboard"], build);
  }
  execFileSync("go", ["build", "-o", bin, "./cmd/paymux"], build);
  execFileSync("go", ["build", "-o", seedBin, "./cmd/paymux-e2eseed"], build);

  const env = {
    ...process.env,
    PAYMUX_ENV: "development",
    PAYMUX_BASE_URL: `http://127.0.0.1:${port}`,
    PAYMUX_LISTEN: `127.0.0.1:${port}`,
    PAYMUX_INTERNAL_LISTEN: `127.0.0.1:${port + 100}`,
    PAYMUX_DATA_DIR: dataDir,
    PAYMUX_MASTER_KEY: randomBytes(32).toString("base64"),
    PAYMUX_DEV_SKIP_2FA: "true",
    PAYMUX_TZ: "Asia/Kolkata",
  };
  const run = (cmd: string, args: string[], input?: string) =>
    execFileSync(cmd, args, {
      cwd: root,
      env,
      input,
      stdio: ["pipe", "inherit", "inherit"],
    });

  run(bin, ["migrate"]);
  run(
    bin,
    [
      "user",
      "create",
      "--email",
      OWNER_EMAIL,
      "--role",
      "admin",
      "--password-stdin",
    ],
    OWNER_PASSWORD + "\n",
  );
  run(bin, ["app", "create", "--name", "Demo", "--slug", "demo"]);
  const today = new Intl.DateTimeFormat("en-CA", {
    timeZone: "Asia/Kolkata",
  }).format(new Date());
  run(bin, [
    "fx",
    "set",
    "--currency",
    "USD",
    "--date",
    today,
    "--rate",
    "83.2500",
  ]);
  run(seedBin, []);

  const server: ChildProcess = spawn(bin, ["serve"], {
    cwd: root,
    env,
    stdio: process.env.PAYMUX_E2E_VERBOSE ? "inherit" : "ignore",
  });
  const exited = new Promise<never>((_, reject) =>
    server.once("exit", (code) =>
      reject(new Error(`paymux serve exited with ${code}`)),
    ),
  );
  const ready = (async () => {
    for (let i = 0; i < 150; i++) {
      try {
        if ((await fetch(`http://127.0.0.1:${port}/dashboard/login`)).ok)
          return;
      } catch {
        /* not listening yet */
      }
      await new Promise((r) => setTimeout(r, 200));
    }
    throw new Error("paymux serve did not come up");
  })();
  try {
    await Promise.race([ready, exited]);
  } catch (err) {
    server.kill();
    throw err;
  }
  server.removeAllListeners("exit");

  return async () => {
    server.kill("SIGTERM");
    await new Promise((r) => setTimeout(r, 300));
    rmSync(dataDir, { recursive: true, force: true });
  };
}
