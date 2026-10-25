import { expect, test } from "vitest";
import { createMemoryHistory } from "@tanstack/react-router";
import { createDashboardRouter } from "./shell";

// The server reads query values as written ("unsaved=1"); the router must not
// turn them into JSON ("%221%22") when it navigates.
test("query values stay exactly as they are through the router", async () => {
  const router = createDashboardRouter(
    createMemoryHistory({
      initialEntries: [
        "/dashboard/reauth?next=%2Fdashboard%2Fappearance&unsaved=1",
      ],
    }),
  );
  await router.load();
  expect(router.state.location.searchStr).toBe(
    "?next=%2Fdashboard%2Fappearance&unsaved=1",
  );
  await router.navigate({
    to: "/dashboard/reauth",
    search: {
      next: "/dashboard/appearance",
      unsaved: "1",
      page: "2",
      ok: "true",
    },
  });
  expect(router.state.location.searchStr).toBe(
    "?next=%2Fdashboard%2Fappearance&unsaved=1&page=2&ok=true",
  );
});
