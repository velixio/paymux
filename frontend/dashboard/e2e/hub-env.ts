import { randomBytes } from "node:crypto";
import { join } from "node:path";

// The environment of the hub under test. It keeps nothing of the developer's
// Paymux configuration, from the shell or from .env, so the tests can only ever
// touch the throw-away data directory: a database URL left in .env would
// otherwise send them to a real database. PAYMUX_ENV_FILE names a file that does
// not exist, which the hub reads as an empty one.
export function hubEnv(
  inherited: NodeJS.ProcessEnv,
  dataDir: string,
  port: number,
): NodeJS.ProcessEnv {
  const kept = Object.fromEntries(
    Object.entries(inherited).filter(([key]) => !key.startsWith("PAYMUX_")),
  );
  return {
    ...kept,
    PAYMUX_ENV_FILE: join(dataDir, "no-such.env"),
    PAYMUX_ENV: "development",
    PAYMUX_BASE_URL: `http://127.0.0.1:${port}`,
    PAYMUX_LISTEN: `127.0.0.1:${port}`,
    PAYMUX_INTERNAL_LISTEN: `127.0.0.1:${port + 100}`,
    PAYMUX_DATA_DIR: dataDir,
    PAYMUX_MASTER_KEY: randomBytes(32).toString("base64"),
    PAYMUX_DEV_SKIP_2FA: "true",
    PAYMUX_TZ: "Asia/Kolkata",
  };
}
