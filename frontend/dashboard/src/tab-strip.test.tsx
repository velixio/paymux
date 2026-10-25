import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { api, link, mount, resetHost, tabByHref } from "./host.test.helpers";

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const hrefs = () => api.state.tabs.map((t) => t.href);
const tab = (name: string | RegExp) => screen.getByRole("tab", { name });
const key = (code: string, init: KeyboardEventInit = {}) =>
  fireEvent.keyDown(document.body, { code, key: code, ...init });

async function threeTabs() {
  await mount();
  act(() => api.openHref("/dashboard/payments", "new"));
  await screen.findByText("Payments body");
  act(() => api.openHref("/dashboard/apps", "new"));
  await screen.findByText("Apps body");
}

test("renders pinned tabs icon-only with aria-label = title", async () => {
  await mount();
  const overview = tab("Overview");
  expect(overview.getAttribute("aria-label")).toBe("Overview");
  expect(overview.textContent).toBe("");
  expect(overview.getAttribute("aria-selected")).toBe("true");
  act(() => api.openHref("/dashboard/payments", "new"));
  await screen.findByText("Payments body");
  expect(tab("Payments").textContent).toContain("Payments");
  expect(tab("Payments").getAttribute("aria-selected")).toBe("true");
  expect(tab("Overview").getAttribute("aria-selected")).toBe("false");
});

test("context menu offers Close, Close others, Close to the right, Pin/Unpin, Duplicate, Copy link, Reload", async () => {
  await threeTabs();
  fireEvent.contextMenu(tab("Payments"));
  const menu = await screen.findByRole("menu");
  expect(
    within(menu)
      .getAllByRole("menuitem")
      .map((i) => i.textContent),
  ).toEqual([
    "Close",
    "Close others",
    "Close to the right",
    "Pin",
    "Duplicate",
    "Copy link",
    "Reload",
  ]);
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Pin" }));
  await waitFor(() =>
    expect(tabByHref("/dashboard/payments").pinned).toBe(true),
  );
  fireEvent.contextMenu(screen.getByRole("tab", { name: "Payments" }));
  expect(
    (await screen.findByRole("menuitem", { name: "Unpin" })).textContent,
  ).toBe("Unpin");
});

test("context menu actions: duplicate, close to the right, close others, close", async () => {
  await threeTabs();
  fireEvent.contextMenu(tab("Payments"));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Duplicate" }));
  await waitFor(() => expect(api.state.tabs).toHaveLength(4));
  expect(hrefs()).toEqual([
    "/dashboard/overview",
    "/dashboard/payments",
    "/dashboard/payments",
    "/dashboard/apps",
  ]);
  fireEvent.contextMenu(screen.getAllByRole("tab", { name: "Payments" })[0]);
  fireEvent.click(
    await screen.findByRole("menuitem", { name: "Close to the right" }),
  );
  await waitFor(() => expect(api.state.tabs).toHaveLength(2));
  fireEvent.contextMenu(tab("Payments"));
  fireEvent.click(
    await screen.findByRole("menuitem", { name: "Close others" }),
  );
  await waitFor(() =>
    expect(hrefs()).toEqual(["/dashboard/overview", "/dashboard/payments"]),
  );
  fireEvent.contextMenu(tab("Payments"));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Close" }));
  await waitFor(() => expect(hrefs()).toEqual(["/dashboard/overview"]));
});

test("copy link and reload", async () => {
  const writeText = vi.fn(async () => {});
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  await threeTabs();
  fireEvent.contextMenu(tab("Payments"));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Copy link" }));
  await waitFor(() =>
    expect(writeText).toHaveBeenCalledWith(
      location.origin + "/dashboard/payments",
    ),
  );
  const before = vi.mocked(fetch).mock.calls.length;
  fireEvent.contextMenu(tab("Apps"));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Reload" }));
  await waitFor(() =>
    expect(vi.mocked(fetch).mock.calls.length).toBeGreaterThan(before),
  );
});

test("[ and ] move between tabs; g 1 activates first; g w closes; g t reopens", async () => {
  await threeTabs();
  const active = () =>
    api.state.tabs.find((t) => t.id === api.state.activeId)!.href;
  expect(active()).toBe("/dashboard/apps");
  key("BracketLeft");
  await waitFor(() => expect(active()).toBe("/dashboard/payments"));
  key("BracketRight");
  await waitFor(() => expect(active()).toBe("/dashboard/apps"));
  key("KeyG");
  key("Digit1");
  await waitFor(() => expect(active()).toBe("/dashboard/overview"));
  key("BracketRight");
  key("BracketRight");
  await waitFor(() => expect(active()).toBe("/dashboard/apps"));
  key("KeyG");
  key("KeyW");
  await waitFor(() => expect(hrefs()).toHaveLength(2));
  key("KeyG");
  key("KeyT");
  await waitFor(() => expect(active()).toBe("/dashboard/apps"));
  expect(hrefs()).toHaveLength(3);
});

test("shortcuts are ignored while typing", async () => {
  await threeTabs();
  const input = document.createElement("input");
  document.body.append(input);
  fireEvent.keyDown(input, { code: "BracketLeft", key: "[" });
  fireEvent.keyDown(input, { code: "BracketLeft", key: "[" });
  expect(api.state.activeId).toBe(tabByHref("/dashboard/apps").id);
  input.remove();
});

test("alt-click on a workspace link opens a new tab, plain click replaces", async () => {
  await mount();
  act(() => api.openHref("/dashboard/payments", "new"));
  await screen.findByText("Payments body");
  fireEvent.click(link("Apps"));
  await screen.findByText("Apps body");
  expect(hrefs()).toEqual(["/dashboard/overview", "/dashboard/apps"]);
  fireEvent.click(link("Payments"), { altKey: true });
  await screen.findByText("Payments body");
  expect(hrefs()).toEqual([
    "/dashboard/overview",
    "/dashboard/apps",
    "/dashboard/payments",
  ]);
});

test("ctrl-click is not intercepted", async () => {
  await mount();
  const untouched = fireEvent.click(link("Payments"), { ctrlKey: true });
  expect(untouched).toBe(true);
  const meta = fireEvent.click(link("Payments"), { metaKey: true });
  expect(meta).toBe(true);
  expect(hrefs()).toEqual(["/dashboard/overview"]);
});

test("middle-click on a tab closes it", async () => {
  await threeTabs();
  fireEvent(
    tab("Payments"),
    new MouseEvent("auxclick", { button: 1, bubbles: true, cancelable: true }),
  );
  await waitFor(() =>
    expect(hrefs()).toEqual(["/dashboard/overview", "/dashboard/apps"]),
  );
});

test("the close button closes an unpinned tab; pinned tabs have none", async () => {
  await threeTabs();
  expect(screen.queryByRole("button", { name: "Close Overview" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Close Apps" }));
  await waitFor(() => expect(api.state.tabs).toHaveLength(2));
});

test("below 640px the strip renders an N open button that opens a sheet listing tabs", async () => {
  vi.stubGlobal("matchMedia", (query: string) => ({
    matches: query.includes("639"),
    media: query,
    addEventListener() {},
    removeEventListener() {},
    addListener() {},
    removeListener() {},
  }));
  await mount();
  act(() => api.openHref("/dashboard/payments", "new"));
  await screen.findByText("Payments body");
  expect(screen.queryByRole("tablist")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "2 open" }));
  const sheet = await screen.findByRole("dialog");
  fireEvent.click(within(sheet).getByRole("button", { name: "Overview" }));
  await waitFor(() =>
    expect(api.state.activeId).toBe(tabByHref("/dashboard/overview").id),
  );
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

test("drag reorders", async () => {
  await threeTabs();
  const tabs = screen.getAllByRole("tab");
  tabs.forEach((el, i) =>
    vi.spyOn(el, "getBoundingClientRect").mockReturnValue({
      left: i * 100,
      right: i * 100 + 100,
      width: 100,
      top: 0,
      bottom: 34,
      height: 34,
      x: i * 100,
      y: 0,
      toJSON() {},
    }),
  );
  fireEvent.pointerDown(tabs[2], { clientX: 250, button: 0, pointerId: 1 });
  fireEvent.pointerMove(window, { clientX: 150, pointerId: 1 });
  fireEvent.pointerUp(window, { clientX: 150, pointerId: 1 });
  await waitFor(() =>
    expect(hrefs()).toEqual([
      "/dashboard/overview",
      "/dashboard/apps",
      "/dashboard/payments",
    ]),
  );
});

test("a click without movement does not drag", async () => {
  await threeTabs();
  const tabs = screen.getAllByRole("tab");
  fireEvent.pointerDown(tabs[1], { clientX: 150, button: 0, pointerId: 1 });
  fireEvent.pointerUp(window, { clientX: 150, pointerId: 1 });
  fireEvent.click(tabs[1]);
  await waitFor(() =>
    expect(api.state.activeId).toBe(tabByHref("/dashboard/payments").id),
  );
  expect(hrefs()).toEqual([
    "/dashboard/overview",
    "/dashboard/payments",
    "/dashboard/apps",
  ]);
});
