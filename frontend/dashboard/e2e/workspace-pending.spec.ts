import { test } from "@playwright/test";

// The Task 14 flow that waits for the command bar (the tab flows live in
// tabs.spec.ts, the chart flows in workspace.spec.ts). It becomes a real test
// when the command bar is merged.
test.describe("pending frontend-lane flows", () => {
  test.fixme("ctrl+k finds a payment id and opens it", async () => {});
});
