import { test } from "@playwright/test";

// Flows of Task 14 that wait for the chart and the command bar (the tab flows
// live in tabs.spec.ts). Each becomes a real test when its feature is merged.
test.describe("pending frontend-lane flows", () => {
  test.fixme("ctrl+k finds a payment id and opens it", async () => {});
  test.fixme("overview shows chart", async () => {});
  test.fixme("switch to 12M shows monthly bars", async () => {});
});
