import { describe, expect, it } from "vitest";
import { hubEnv } from "./hub-env";

const dataDir = "/tmp/paymux-e2e-x";

describe("hubEnv", () => {
  it("drops every Paymux setting of the developer's shell", () => {
    const env = hubEnv(
      {
        PAYMUX_DATABASE_URL: "postgres://dev/paymux_dev",
        PAYMUX_MASTER_KEY_FILE: "/secrets/master.key",
      },
      dataDir,
      8093,
    );
    expect(env).not.toHaveProperty("PAYMUX_DATABASE_URL");
    expect(env).not.toHaveProperty("PAYMUX_MASTER_KEY_FILE");
  });

  it("points the dotenv file inside the throw-away data directory", () => {
    const env = hubEnv({ PAYMUX_ENV_FILE: ".env" }, dataDir, 8093);
    expect(env.PAYMUX_ENV_FILE).toBe(`${dataDir}/no-such.env`);
  });

  it("keeps what the tools need and sets the hub's own address", () => {
    const env = hubEnv({ PATH: "/usr/bin" }, dataDir, 8093);
    expect(env.PATH).toBe("/usr/bin");
    expect(env.PAYMUX_DATA_DIR).toBe(dataDir);
    expect(env.PAYMUX_LISTEN).toBe("127.0.0.1:8093");
    expect(env.PAYMUX_INTERNAL_LISTEN).toBe("127.0.0.1:8193");
  });
});
