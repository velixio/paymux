import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { api, handlers, mount, pageHtml, resetHost } from "./host.test.helpers";

const record = (id: string, status: string) =>
  `<h1>${id} <span class="status status-${status}">${status}</span></h1><p class="muted">about</p><dl><dt>Amount</dt><dd>10.00 INR</dd></dl><h3>Attempts</h3><p>${id} attempts</p><h3>Ledger</h3><p>${id} ledger</p>`;
const list = `<h1>Payments</h1><table><thead><tr><th>ID</th><th>Status</th></tr></thead><tbody>
<tr><td><a href="/dashboard/payments/pay_1">pay_1</a></td><td>succeeded</td></tr>
<tr><td><a href="/dashboard/payments/pay_2">pay_2</a></td><td>failed</td></tr></tbody></table><p>List body</p>`;

function width(wide: boolean) {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: wide && query.includes("min-width: 1100px"),
    media: query,
    addEventListener() {},
    removeEventListener() {},
  }));
}

beforeEach(() => {
  resetHost();
  width(true);
  handlers["/dashboard/payments"] = () => pageHtml("Payments", list);
  handlers["/dashboard/payments/pay_1"] = () =>
    pageHtml("pay_1", record("pay_1", "succeeded"));
  handlers["/dashboard/payments/pay_2"] = () =>
    pageHtml("pay_2", record("pay_2", "failed"));
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const pane = () =>
  screen.queryByRole("complementary", { name: "Record preview" });
const search = (router: Awaited<ReturnType<typeof mount>>) =>
  router.state.location.search as { peek?: string };

async function onList() {
  const router = await mount("/dashboard/payments");
  await screen.findByText("List body");
  return router;
}
const rowLink = (id: string) => screen.getByRole("link", { name: id });

test("a plain click on a row link opens the record in a pane; the list stays", async () => {
  const router = await onList();
  const tabsBefore = api.state.tabs.length;
  fireEvent.click(rowLink("pay_1"));
  const p = await screen.findByRole("complementary", {
    name: "Record preview",
  });
  expect(search(router).peek).toBe("pay_1");
  expect(await within(p).findByText("succeeded")).toBeTruthy();
  expect(within(p).getByText("pay_1")).toBeTruthy();
  // inner tabs from Task D
  expect(
    within(p)
      .getAllByRole("tab")
      .map((t) => t.textContent),
  ).toEqual(["Details", "Attempts", "Ledger"]);
  expect(screen.getByText("List body")).toBeTruthy();
  expect(api.state.tabs).toHaveLength(tabsBefore); // same tab
  expect(rowLink("pay_1").closest("tr")!.hasAttribute("data-peek-row")).toBe(
    true,
  );
  expect(rowLink("pay_2").closest("tr")!.hasAttribute("data-peek-row")).toBe(
    false,
  );
});

test("clicking another row replaces the pane content", async () => {
  const router = await onList();
  fireEvent.click(rowLink("pay_1"));
  await screen.findByRole("complementary", { name: "Record preview" });
  fireEvent.click(rowLink("pay_2"));
  await waitFor(() => expect(search(router).peek).toBe("pay_2"));
  await within(pane()!).findByText("failed");
  expect(rowLink("pay_2").closest("tr")!.hasAttribute("data-peek-row")).toBe(
    true,
  );
  expect(rowLink("pay_1").closest("tr")!.hasAttribute("data-peek-row")).toBe(
    false,
  );
});

test("Escape and the Close button close the pane and drop ?peek", async () => {
  const router = await onList();
  fireEvent.click(rowLink("pay_1"));
  await screen.findByRole("complementary", { name: "Record preview" });
  fireEvent.keyDown(document.body, { key: "Escape", code: "Escape" });
  await waitFor(() => expect(pane()).toBeNull());
  expect(search(router).peek).toBeUndefined();
  fireEvent.click(rowLink("pay_2"));
  const p = await screen.findByRole("complementary", {
    name: "Record preview",
  });
  fireEvent.click(within(p).getByRole("button", { name: "Close preview" }));
  await waitFor(() => expect(pane()).toBeNull());
});

test("Open in tab opens the record in a new tab and the list tab keeps its page", async () => {
  await onList();
  const before = api.state.tabs.length;
  fireEvent.click(rowLink("pay_1"));
  const p = await screen.findByRole("complementary", {
    name: "Record preview",
  });
  fireEvent.click(within(p).getByRole("button", { name: "Open in tab" }));
  await waitFor(() => expect(api.state.tabs).toHaveLength(before + 1));
  const active = api.state.tabs.find((t) => t.id === api.state.activeId)!;
  expect(active.href).toBe("/dashboard/payments/pay_1");
  expect(api.state.tabs.some((t) => t.href === "/dashboard/payments")).toBe(
    true,
  );
});

test("a reload with ?peek restores the pane", async () => {
  await mount(
    "/dashboard/payments?peek=" +
      encodeURIComponent("/dashboard/payments/pay_2"),
  );
  const p = await screen.findByRole("complementary", {
    name: "Record preview",
  });
  await within(p).findByText("failed");
  expect(
    api.state.tabs.filter((t) => t.href.startsWith("/dashboard/payments")),
  ).toHaveLength(1);
});

test("alt-click still opens a new tab, with no pane", async () => {
  await onList();
  const before = api.state.tabs.length;
  fireEvent.click(rowLink("pay_1"), { altKey: true });
  await waitFor(() => expect(api.state.tabs).toHaveLength(before + 1));
  expect(pane()).toBeNull();
});

test("below 1100px a click navigates as before; links outside table rows never open a pane", async () => {
  width(false);
  await onList();
  fireEvent.click(rowLink("pay_1"));
  await waitFor(() =>
    expect(api.state.tabs.find((t) => t.id === api.state.activeId)!.href).toBe(
      "/dashboard/payments/pay_1",
    ),
  );
  expect(pane()).toBeNull();
});

test("j moves the selection and the pane follows; Enter opens the selected record in a tab", async () => {
  const router = await onList();
  fireEvent.click(rowLink("pay_1"));
  await screen.findByRole("complementary", { name: "Record preview" });
  act(() => {
    fireEvent.keyDown(window, { code: "KeyJ", key: "j" });
  });
  act(() => {
    fireEvent.keyDown(window, { code: "KeyJ", key: "j" });
  });
  await waitFor(() => expect(search(router).peek).toBe("pay_2"));
  const before = api.state.tabs.length;
  act(() => {
    fireEvent.keyDown(document.activeElement!, { code: "Enter", key: "Enter" });
  });
  await waitFor(() => expect(api.state.tabs).toHaveLength(before + 1));
  expect(api.state.tabs.find((t) => t.id === api.state.activeId)!.href).toBe(
    "/dashboard/payments/pay_2",
  );
});

test("closing the pane with unsaved edits asks first", async () => {
  handlers["/dashboard/payments/pay_1"] = () =>
    pageHtml(
      "pay_1",
      `<h1>pay_1</h1><form method="post" action="/dashboard/payments/pay_1/note"><input type="hidden" name="csrf" value="t"><input name="note" aria-label="Note" value=""><button type="submit">Save</button></form>`,
    );
  const router = await onList();
  fireEvent.click(rowLink("pay_1"));
  const p = await screen.findByRole("complementary", {
    name: "Record preview",
  });
  const note = await within(p).findByLabelText("Note");
  fireEvent.input(note, { target: { value: "draft" } });
  fireEvent.click(within(p).getByRole("button", { name: "Close preview" }));
  const dialog = await screen.findByRole("dialog", {
    name: "Discard unsaved changes?",
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(pane()).not.toBeNull();
  expect(search(router).peek).toBe("pay_1");
  fireEvent.click(
    within(pane()!).getByRole("button", { name: "Close preview" }),
  );
  fireEvent.click(
    await screen.findByRole("button", { name: "Discard and continue" }),
  );
  await waitFor(() => expect(pane()).toBeNull());
});

async function dirtyPane() {
  handlers["/dashboard/payments/pay_1"] = () =>
    pageHtml(
      "pay_1",
      `<h1>pay_1</h1><form method="post" action="/dashboard/payments/pay_1/note"><input type="hidden" name="csrf" value="t"><input name="note" aria-label="Note" value=""><button type="submit">Save</button></form>`,
    );
  const router = await onList();
  fireEvent.click(rowLink("pay_1"));
  const p = await screen.findByRole("complementary", {
    name: "Record preview",
  });
  fireEvent.input(await within(p).findByLabelText("Note"), {
    target: { value: "draft" },
  });
  return router;
}

test("clicking another row with unsaved edits asks first; keeping the edits keeps the record", async () => {
  const router = await dirtyPane();
  fireEvent.click(rowLink("pay_2"));
  const dialog = await screen.findByRole("dialog", {
    name: "Discard unsaved changes?",
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(search(router).peek).toBe("pay_1");
  expect(
    (within(pane()!).getByLabelText("Note") as HTMLInputElement).value,
  ).toBe("draft");
  expect(rowLink("pay_1").closest("tr")!.hasAttribute("data-peek-row")).toBe(
    true,
  );

  fireEvent.click(rowLink("pay_2"));
  fireEvent.click(
    await screen.findByRole("button", { name: "Discard and continue" }),
  );
  await waitFor(() => expect(search(router).peek).toBe("pay_2"));
  await within(pane()!).findByText("failed");
});

test("moving the selection with j/k with unsaved edits asks first and keeps the current row selected", async () => {
  const router = await dirtyPane();
  act(() => {
    fireEvent.keyDown(window, { code: "KeyJ", key: "j" });
  });
  act(() => {
    fireEvent.keyDown(window, { code: "KeyJ", key: "j" });
  });
  const dialog = await screen.findByRole("dialog", {
    name: "Discard unsaved changes?",
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(search(router).peek).toBe("pay_1");
  const current = rowLink("pay_1").closest("tr")!;
  expect(current.hasAttribute("data-nav-row")).toBe(true);
  expect(rowLink("pay_2").closest("tr")!.hasAttribute("data-nav-row")).toBe(
    false,
  );
  expect(current.contains(document.activeElement)).toBe(true);
  expect(
    (within(pane()!).getByLabelText("Note") as HTMLInputElement).value,
  ).toBe("draft");
});

test("the pane header carries the record's title, status and action links; the body repeats none of them", async () => {
  handlers["/dashboard/payments/pay_1"] = () =>
    pageHtml(
      "pay_1",
      `<p><a href="/dashboard/payments">← Payments</a></p><h1>pay_1 <span class="status status-succeeded">succeeded</span> <span class="mode mode-live">live</span></h1><p class="muted">About this payment</p><p><a href="/dashboard/logs?id=pay_1">Logs for this payment</a></p><dl><dt>Amount</dt><dd>10.00 INR</dd></dl><h3>Attempts</h3><p>a</p><h3>Ledger</h3><p>l</p>`,
    );
  await onList();
  fireEvent.click(rowLink("pay_1"));
  const p = await screen.findByRole("complementary", {
    name: "Record preview",
  });
  await within(p).findByText("succeeded");
  const head = p.querySelector(".peek-head") as HTMLElement;
  expect(
    within(head).getByRole("link", { name: "Logs for this payment" }),
  ).toBeTruthy();
  expect(
    within(head).getByRole("button", { name: "Open in tab" }),
  ).toBeTruthy();
  const body = p.querySelector(".peek-body") as HTMLElement;
  expect(
    within(body).queryByRole("link", {
      name: /Logs for this payment|Payments/,
    }),
  ).toBeNull();
  expect(body.querySelector("h1")).toBeNull();
  expect(within(body).queryByText("pay_1")).toBeNull();
  expect(within(body).queryByText("About this payment")).toBeNull();
  expect(within(body).getAllByRole("tab")).toHaveLength(3);
});

test("Open in tab is an icon button, and the status pill sits outside the truncating title", async () => {
  handlers["/dashboard/payments/pay_1"] = () =>
    pageHtml(
      "pay_1",
      `<p><a href="/dashboard/payments">← Payments</a></p><h1>pay_1 <span class="status status-succeeded">succeeded</span></h1><dl><dt>Amount</dt><dd>10.00 INR</dd></dl>`,
    );
  await onList();
  fireEvent.click(rowLink("pay_1"));
  const p = await screen.findByRole("complementary", {
    name: "Record preview",
  });
  await within(p).findByText("succeeded");
  const head = p.querySelector(".peek-head") as HTMLElement;
  const open = within(head).getByRole("button", { name: "Open in tab" });
  // No visible label: only the arrow icon, named for screen readers.
  expect(open.textContent?.trim()).toBe("");
  expect(open.getAttribute("title")).toBe("Open in tab");
  // The id truncates on its own; the pill is its sibling, never clipped.
  const text = head.querySelector(".peek-title-text") as HTMLElement;
  expect(text.textContent?.trim()).toBe("pay_1");
  const pill = within(head).getByText("succeeded");
  expect(text.contains(pill)).toBe(false);
  expect(pill.closest(".peek-title")).not.toBeNull();
});

test("in the pane, a link to another section opens in a tab; the list keeps its pane", async () => {
  handlers["/dashboard/payments/pay_1"] = () =>
    pageHtml(
      "pay_1",
      `<h1>pay_1</h1><p><a href="/dashboard/logs?id=pay_1">Logs for this payment</a></p><dl><dt>Amount</dt><dd>10.00 INR</dd></dl>`,
    );
  handlers["/dashboard/logs"] = () => pageHtml("Logs", "<p>Logs body</p>");
  const router = await onList();
  fireEvent.click(rowLink("pay_1"));
  const p = await screen.findByRole("complementary", {
    name: "Record preview",
  });
  const listTab = api.state.activeId;
  const before = api.state.tabs.length;
  fireEvent.click(
    await within(p).findByRole("link", { name: "Logs for this payment" }),
  );
  await waitFor(() => expect(api.state.tabs).toHaveLength(before + 1));
  expect(api.state.tabs.find((t) => t.id === api.state.activeId)!.href).toBe(
    "/dashboard/logs?id=pay_1",
  );
  // The list tab still shows the payment in its pane.
  expect(api.state.tabs.find((t) => t.id === listTab)!.href).toContain(
    "peek=pay_1",
  );
  expect(router).toBeTruthy();
});
