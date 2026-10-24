import { test } from "@playwright/test";

// Flows of Task 14 that need the workspace tabs, chart, command bar and visual
// system from the frontend lane. They stay fixme until that lane is merged;
// then each becomes a real test in workspace.spec.ts.
test.describe("pending frontend-lane flows", () => {
  test.fixme("open payment in new tab with alt-click, switch tabs keeps filter, reload restores tabs", async () => {});
  test.fixme("dirty form in hidden tab survives switching and blocks close", async () => {});
  test.fixme("ctrl+k finds a payment id and opens it", async () => {});
  test.fixme("test mode shows the top border and TEST badge", async () => {});
  test.fixme("phone width shows the open-tabs sheet", async () => {});
  test.fixme("no request fires from a hidden tab for 60s", async () => {});
  test.fixme("overview shows chart", async () => {});
  test.fixme("switch to 12M shows monthly bars", async () => {});
});
