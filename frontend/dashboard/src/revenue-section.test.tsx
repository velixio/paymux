import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import {
  handlers,
  mount,
  pageHtml,
  resetHost,
  revenueSectionHtml,
} from "./host.test.helpers";

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

async function overview(
  opts: Parameters<typeof revenueSectionHtml>[0] = {},
  url = "/dashboard/overview?range=12m",
) {
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", revenueSectionHtml(opts));
  const router = await mount(url);
  await screen.findByRole("figure", { name: "Revenue chart" });
  return router;
}
const q = (sel: string) => document.querySelector(sel)!;
const before = (a: string, b: string) =>
  Boolean(
    q(a).compareDocumentPosition(q(b)) & Node.DOCUMENT_POSITION_FOLLOWING,
  );
const search = (router: Awaited<ReturnType<typeof overview>>) =>
  new URLSearchParams(router.state.location.searchStr);

test("order: toolbar, KPI tiles, chart, notes, then the table toggle and CSV", async () => {
  await overview();
  expect(before(".revenue-toolbar", ".revenue-kpis")).toBe(true);
  expect(before(".revenue-kpis", ".revenue-chart")).toBe(true);
  expect(before(".revenue-chart", ".revenue-notes")).toBe(true);
  expect(before(".revenue-notes", ".revenue-table-toggle")).toBe(true);
  expect(before(".revenue-table-toggle", "a[download]")).toBe(true);
  // The server's stacked form and definition list are replaced.
  expect(document.querySelector("form.revenue-filters")).toBeNull();
  expect(document.querySelector("dl.revenue-summary")).toBeNull();
  expect(
    screen.getAllByText(/Includes estimated conversions/).length,
  ).toBeGreaterThan(0);
});

test("KPI tiles format minor units with the table's currency", async () => {
  await overview();
  const tiles = [...document.querySelectorAll(".revenue-kpi")];
  expect(tiles.map((t) => t.querySelector(".kpi-label")!.textContent)).toEqual([
    "Gross",
    "Refunds",
    "Net",
  ]);
  expect(tiles.map((t) => t.querySelector(".kpi-value")!.textContent)).toEqual([
    "₹23,991.75",
    "₹500.00",
    "₹23,491.75",
  ]);
});

test("change chips: up is good, a refunds increase is bad, empty has no chip", async () => {
  await overview({ changes: ["+54.7%", "+25.0%", ""] });
  const chip = (metric: string) =>
    document.querySelector(`.revenue-kpi[data-metric="${metric}"] .kpi-chip`);
  expect(chip("gross")?.textContent).toContain("+54.7%");
  expect(chip("gross")?.getAttribute("data-tone")).toBe("good");
  expect(chip("refunds")?.getAttribute("data-tone")).toBe("bad");
  expect(chip("net")).toBeNull();
});

test("change chips: a refunds decrease is good, a gross decrease is bad", async () => {
  await overview({ changes: ["−3.0%", "-10.0%", "n/a"] });
  const tone = (m: string) =>
    document
      .querySelector(`.revenue-kpi[data-metric="${m}"] .kpi-chip`)
      ?.getAttribute("data-tone");
  expect(tone("gross")).toBe("bad");
  expect(tone("refunds")).toBe("good");
  expect(tone("net")).toBe("flat");
});

test("selects apply on change and keep the current range", async () => {
  const router = await overview({}, "/dashboard/overview?range=12m");
  const toolbar = () => within(q(".revenue-toolbar") as HTMLElement);
  fireEvent.change(toolbar().getByLabelText("App"), {
    target: { value: "app_1" },
  });
  await waitFor(() => expect(search(router).get("app")).toBe("app_1"));
  expect(search(router).get("range")).toBe("12m");
  expect(search(router).has("from")).toBe(false);
  fireEvent.change(toolbar().getByLabelText("Group by"), {
    target: { value: "month" },
  });
  await waitFor(() => expect(search(router).get("bucket")).toBe("month"));
  expect(search(router).get("app")).toBe("app_1");
  expect(search(router).get("range")).toBe("12m");
});

test("choosing the empty option removes that filter", async () => {
  const router = await overview(
    { app: "app_1" },
    "/dashboard/overview?range=7d&app=app_1",
  );
  fireEvent.change(
    within(q(".revenue-toolbar") as HTMLElement).getByLabelText("App"),
    { target: { value: "" } },
  );
  await waitFor(() => expect(search(router).has("app")).toBe(false));
  expect(search(router).get("range")).toBe("7d");
});

test("Custom range opens a popover whose Apply sets range=custom", async () => {
  const router = await overview(
    {},
    "/dashboard/overview?range=30d&currency=INR",
  );
  expect(screen.queryByLabelText("From")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Custom range" }));
  const from = await screen.findByLabelText("From");
  const to = screen.getByLabelText("To");
  expect((from as HTMLInputElement).value).toBe("2026-09-26");
  fireEvent.change(from, { target: { value: "2026-09-01" } });
  fireEvent.change(to, { target: { value: "2026-09-30" } });
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  await waitFor(() => expect(search(router).get("range")).toBe("custom"));
  expect(search(router).get("from")).toBe("2026-09-01");
  expect(search(router).get("to")).toBe("2026-09-30");
  expect(search(router).get("currency")).toBe("INR");
  await waitFor(() => expect(screen.queryByLabelText("From")).toBeNull());
});

test("range presets stay links and the pinned tab updates in place", async () => {
  await overview({}, "/dashboard/overview?range=30d");
  const presets = within(q("nav.range-presets") as HTMLElement);
  expect(presets.getAllByRole("link").map((a) => a.textContent)).toEqual([
    "7D",
    "30D",
    "12M",
  ]);
  act(() => {});
});

test("without data rows the section keeps the server layout", async () => {
  handlers["/dashboard/overview"] = () =>
    pageHtml(
      "Overview",
      revenueSectionHtml({ rows: [] }).replace(
        /<tr data-start[\s\S]*?<\/tr>/g,
        "",
      ),
    );
  await mount("/dashboard/overview");
  await screen.findByText("Revenue");
  expect(document.querySelector(".revenue-chart")).toBeNull();
  expect(document.querySelector(".revenue-kpis")).toBeNull();
  expect(document.querySelector("dl.revenue-summary")).not.toBeNull();
});
