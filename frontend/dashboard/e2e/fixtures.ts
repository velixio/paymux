import { createHash } from "node:crypto";
import { test as base } from "@playwright/test";

export * from "@playwright/test";

// Login attempts are limited per address and minute, and every test signs in.
// The hub trusts a forwarded address from loopback, so each test gets its own and
// the suite's length can't lock a later test out. Only for browser tests.
export const test = base.extend({
  extraHTTPHeaders: async ({}, use, testInfo) => {
    const hash = createHash("sha256").update(testInfo.testId).digest();
    // Outside the hub's trusted networks: it skips those as proxy hops.
    await use({ "X-Forwarded-For": `198.18.${hash[0]}.${hash[1]}` });
  },
});
