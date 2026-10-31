import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { handlers, mount, pageHtml, resetHost } from "./host.test.helpers";

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const q = (selector: string) => document.querySelector(selector) as HTMLElement;
const nav = (extra = "") =>
  `<a href="/dashboard/overview">Overview</a><a href="/dashboard/payments"${extra}>Payments</a><a href="/dashboard/disputes" data-count="2">Disputes</a>`;

test("the title bar holds the brand, the tabs and the controls", async () => {
  await mount();
  const bar = q("header.react-titlebar");
  expect(bar).not.toBeNull();
  expect(within(bar).getByRole("tablist")).toBeTruthy();
  expect(
    within(bar).getByRole("link", { name: "Paymux overview" }),
  ).toBeTruthy();
  expect(
    within(bar).getByRole("button", { name: "Search and commands" }),
  ).toBeTruthy();
  expect(
    within(bar).getByRole("button", { name: "Account and mode" }),
  ).toBeTruthy();
  expect(within(bar).getByText("LIVE")).toBeTruthy();
  // The old breadcrumb row and the separate tab row are gone.
  expect(document.querySelector(".react-header, .breadcrumb-root")).toBeNull();
  expect(q("aside").querySelector(".react-logo")).toBeNull();
});

test("the sidebar drops the brand card and the tagline", async () => {
  await mount();
  const side = q("aside");
  expect(side.textContent).not.toContain("Payment workspace");
  expect(side.textContent).not.toContain("Self-hosted infrastructure");
  expect(document.body.textContent).not.toContain("Your infrastructure");
  expect(document.body.textContent).not.toContain("Paymux control plane");
});

test("the sidebar can still be collapsed from the title bar", async () => {
  await mount();
  fireEvent.click(screen.getByRole("button", { name: "Collapse sidebar" }));
  expect(q(".app-shell").className).toContain("sidebar-collapsed");
  expect(screen.getByRole("button", { name: "Expand sidebar" })).toBeTruthy();
});

test("nav items show a count only when the page carries one", async () => {
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", "<p>Overview body</p>", { nav: nav() });
  await mount();
  const side = within(q("aside"));
  const disputes = side.getByRole("link", { name: /Disputes/ });
  expect(within(disputes).getByText("2").className).toContain("nav-count");
  expect(q("aside").querySelectorAll(".nav-count")).toHaveLength(1);
});

test("the status bar shows the connection", async () => {
  await mount();
  const bar = q(".status-bar");
  expect(within(bar).getByText("Unavailable")).toBeTruthy();
  act(() => {
    window.dispatchEvent(new Event("offline"));
  });
  expect(within(bar).getByText("Offline")).toBeTruthy();
  act(() => {
    window.dispatchEvent(new Event("online"));
  });
  expect(within(bar).getByText("Unavailable")).toBeTruthy();
});

test("the status bar shows when the active tab was last updated", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-25T10:00:00Z"));
  await mount();
  const bar = within(q(".status-bar"));
  expect(bar.getByText("Updated just now")).toBeTruthy();
  vi.setSystemTime(new Date("2026-10-25T10:00:12Z"));
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
  expect(bar.getByText("Updated 12 s ago")).toBeTruthy();
});

test("a warning becomes a status-bar chip that opens a popover", async () => {
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", "<p>Overview body</p>", {
      warnings: ["2FA disabled (development)"],
    });
  await mount();
  expect(document.querySelector(".workspace-warning")).toBeNull();
  const chip = within(q(".status-bar")).getByRole("button", {
    name: /2FA disabled/,
  });
  fireEvent.click(chip);
  const list = await screen.findByRole("list", { name: "Warnings" });
  expect(within(list).getAllByRole("listitem")).toHaveLength(1);
});

test("several warnings collapse to a count", async () => {
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", "<p>Overview body</p>", {
      warnings: ["2FA disabled", "Debug logging on"],
    });
  await mount();
  const chip = within(q(".status-bar")).getByRole("button", {
    name: /2 warnings/,
  });
  fireEvent.click(chip);
  const list = await screen.findByRole("list", { name: "Warnings" });
  expect(within(list).getAllByRole("listitem")).toHaveLength(2);
});

test("no warnings, no chip", async () => {
  await mount();
  expect(
    within(q(".status-bar")).queryByRole("button", { name: /warning/i }),
  ).toBeNull();
});

test("the status bar keeps the shortcut hint and the ? sheet button", async () => {
  await mount();
  const bar = within(q(".status-bar"));
  expect(bar.getByText(/j\/k move/)).toBeTruthy();
  fireEvent.click(bar.getByRole("button", { name: "Keyboard shortcuts" }));
  await screen.findByRole("dialog", { name: "Keyboard shortcuts" });
});

test("API reference lives in the account menu", async () => {
  await mount();
  const trigger = screen.getByRole("button", { name: "Account and mode" });
  fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
  const item = await screen.findByRole("menuitem", { name: "API reference" });
  expect(item.getAttribute("href")).toBe("/docs");
  await waitFor(() => expect(item).toBeTruthy());
});
