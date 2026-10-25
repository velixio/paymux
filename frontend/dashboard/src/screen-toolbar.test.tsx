import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import {
  handlers,
  loginHtml,
  mount,
  pageHtml,
  resetHost,
} from "./host.test.helpers";

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const q = (s: string) => document.querySelector(s) as HTMLElement;
const filters =
  '<form class="filters" method="get" action="/dashboard/payments"><select name="status" aria-label="Status"><option value="">Any status</option><option value="succeeded">succeeded</option><option value="failed">failed</option></select><input type="search" name="q" aria-label="Search"></form><p>List body</p>';

async function payments(body = filters) {
  handlers["/dashboard/payments"] = () =>
    pageHtml(
      "Payments",
      `<h1>Payments <span class="mode mode-live">live</span></h1><p class="muted">Track each payment.</p><a class="btn" href="/dashboard/links">New link</a>${body}`,
    );
  return mount("/dashboard/payments");
}

test("every screen gets one toolbar with the title and no mode span", async () => {
  await payments();
  const bar = q(".screen-toolbar");
  expect(bar).not.toBeNull();
  expect(within(bar).getByRole("heading", { level: 1 }).textContent).toBe(
    "Payments",
  );
  expect(bar.textContent).not.toContain("live");
  expect(document.querySelectorAll(".screen-toolbar")).toHaveLength(1);
  // The toolbar leads the scrolling content so it can stay put.
  expect(q("section.react-content").firstElementChild).toBe(bar);
  expect(screen.getByText("List body")).toBeTruthy();
});

test("the description lives behind an info button", async () => {
  await payments();
  expect(screen.queryByText("Track each payment.")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "About this page" }));
  expect(await screen.findByText("Track each payment.")).toBeTruthy();
});

test("actions sit on the right, the first one primary", async () => {
  await payments();
  const actions = within(q(".toolbar-actions"));
  const link = actions.getByRole("link", { name: "New link" });
  expect(link.className).toContain("btn");
  expect(q(".screen-toolbar").lastElementChild).toBe(q(".toolbar-actions"));
});

test("status chips apply the filter, All clears it", async () => {
  const router = await payments();
  const group = () => screen.getByRole("group", { name: "Status" });
  const chips = within(group()).getAllByRole("button");
  expect(chips.map((c) => c.textContent)).toEqual([
    "All",
    "succeeded",
    "failed",
  ]);
  expect(chips[0].getAttribute("aria-pressed")).toBe("true");
  fireEvent.click(chips[1]);
  await waitFor(() =>
    expect(router.state.location.searchStr).toBe("?status=succeeded"),
  );
  fireEvent.click(within(group()).getByRole("button", { name: "All" }));
  await waitFor(() => expect(router.state.location.searchStr).toBe(""));
});

test("a chip keeps the other filters in the query", async () => {
  handlers["/dashboard/payments"] = () =>
    pageHtml("Payments", `<h1>Payments</h1>${filters}`);
  const router = await mount("/dashboard/payments?q=alice");
  fireEvent.click(
    within(screen.getByRole("group", { name: "Status" })).getByRole("button", {
      name: "failed",
    }),
  );
  await waitFor(() =>
    expect(router.state.location.searchStr).toContain("status=failed"),
  );
  expect(router.state.location.searchStr).toContain("q=alice");
});

test("record pages get a back button and keep the status pill beside the title", async () => {
  handlers["/dashboard/payments/pay_1"] = () =>
    pageHtml(
      "Payment",
      '<p><a href="/dashboard/payments">&larr; Payments</a></p><h1>pay_1 <span class="status status-paid">paid</span> <span class="mode mode-live">live</span></h1><dl><dt>App</dt><dd>Demo</dd></dl>',
    );
  await mount("/dashboard/payments/pay_1");
  const bar = within(q(".screen-toolbar"));
  const back = bar.getByRole("link", { name: "Back to Payments" });
  expect(back.getAttribute("href")).toBe("/dashboard/payments");
  expect(bar.getByText("paid").className).toContain("pill");
  expect(bar.queryByText("live")).toBeNull();
  expect(screen.getByText("Demo")).toBeTruthy();
});

test("pages without a title and sign-in pages get no toolbar", async () => {
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", "<p>Just text</p>");
  await mount();
  expect(document.querySelector(".screen-toolbar")).toBeNull();
  cleanup();
  handlers["/dashboard/overview"] = () => loginHtml;
  await mount();
  expect(document.querySelector(".screen-toolbar")).toBeNull();
});
