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
  api,
  calls,
  handlers,
  mount,
  pageHtml,
  resetHost,
} from "./host.test.helpers";

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  document.documentElement.classList.remove("dark");
  vi.unstubAllGlobals();
});

const key = (init: KeyboardEventInit & { target?: Element }) =>
  fireEvent.keyDown(init.target ?? document.body, init);
const ctrlK = () => key({ code: "KeyK", key: "k", ctrlKey: true });
const box = () => screen.findByRole("combobox");
const optionNames = () =>
  screen.getAllByRole("option").map((o) => o.textContent ?? "");

test("Ctrl+K opens the command bar even while typing, Escape closes it", async () => {
  await mount();
  const field = document.createElement("input");
  document.body.append(field);
  key({ code: "KeyK", key: "k", ctrlKey: true, target: field });
  await box();
  fireEvent.keyDown(await box(), { key: "Escape", code: "Escape" });
  await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());
  key({ code: "KeyK", key: "k", metaKey: true });
  await box();
  field.remove();
});

test("slash opens it unless the user is typing", async () => {
  await mount();
  const field = document.createElement("input");
  document.body.append(field);
  key({ code: "Slash", key: "/", target: field });
  expect(screen.queryByRole("combobox")).toBeNull();
  key({ code: "Slash", key: "/" });
  await box();
  field.remove();
});

test("the header search button opens it", async () => {
  await mount();
  fireEvent.click(screen.getByRole("button", { name: /search/i }));
  await box();
});

test("combobox: aria-activedescendant follows the arrow keys", async () => {
  await mount();
  ctrlK();
  const input = await box();
  expect(input.getAttribute("aria-expanded")).toBe("true");
  const listbox = screen.getByRole("listbox");
  expect(input.getAttribute("aria-controls")).toBe(listbox.id);
  const options = screen.getAllByRole("option");
  expect(options[0].getAttribute("aria-selected")).toBe("true");
  expect(input.getAttribute("aria-activedescendant")).toBe(options[0].id);
  fireEvent.keyDown(input, { key: "ArrowDown" });
  expect(input.getAttribute("aria-activedescendant")).toBe(options[1].id);
  expect(options[1].getAttribute("aria-selected")).toBe("true");
  fireEvent.keyDown(input, { key: "ArrowUp" });
  expect(input.getAttribute("aria-activedescendant")).toBe(options[0].id);
  // It wraps around.
  fireEvent.keyDown(input, { key: "ArrowUp" });
  expect(input.getAttribute("aria-activedescendant")).toBe(
    options[options.length - 1].id,
  );
});

test("lists actions, pages and open tabs with no query", async () => {
  await mount();
  act(() => api.openHref("/dashboard/payments", "new"));
  await screen.findByText("Payments body");
  ctrlK();
  await box();
  const names = optionNames().join("|");
  expect(names).toContain("Create payment link");
  expect(names).toContain("Toggle theme");
  expect(names).toContain("Switch to test");
  expect(names).toContain("Apps");
  expect(names).not.toContain("Reopen closed tab");
  expect(
    within(screen.getByRole("listbox")).getAllByText("Open tab").length,
  ).toBe(2);
});

test("a payment id opens the record; Enter replaces, Alt+Enter opens a new tab", async () => {
  handlers["/dashboard/payments/pay_e2e_033"] = () =>
    pageHtml("Payment", "<p>Payment pay_e2e_033 body</p>");
  const router = await mount();
  ctrlK();
  fireEvent.change(await box(), { target: { value: "pay_e2e_033" } });
  expect(optionNames()[0]).toContain("Open payment pay_e2e_033");
  fireEvent.keyDown(screen.getByRole("combobox"), { key: "Enter" });
  await screen.findByText("Payment pay_e2e_033 body");
  expect(router.state.location.pathname).toBe(
    "/dashboard/payments/pay_e2e_033",
  );
  await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());

  // Alt+Enter: a new tab even from an unpinned tab.
  const before = api.state.tabs.length;
  ctrlK();
  fireEvent.change(await box(), { target: { value: "Apps" } });
  fireEvent.keyDown(screen.getByRole("combobox"), {
    key: "Enter",
    altKey: true,
  });
  await screen.findByText("Apps body");
  expect(api.state.tabs.length).toBe(before + 1);
});

test("clicking an option runs it", async () => {
  await mount();
  ctrlK();
  fireEvent.click(await screen.findByRole("option", { name: /Apps/ }));
  await screen.findByText("Apps body");
});

test("Toggle theme flips the dark class", async () => {
  await mount();
  const was = document.documentElement.classList.contains("dark");
  ctrlK();
  fireEvent.click(await screen.findByRole("option", { name: /Toggle theme/ }));
  await waitFor(() =>
    expect(document.documentElement.classList.contains("dark")).toBe(!was),
  );
});

test("Switch to test submits the mode form", async () => {
  handlers["/dashboard/mode"] = () =>
    pageHtml("Overview", "<p>Overview body</p>");
  await mount();
  ctrlK();
  fireEvent.click(
    await screen.findByRole("option", { name: /Switch to test/ }),
  );
  await waitFor(() => expect(calls["/dashboard/mode"]).toBe(1));
  const [, init] = vi
    .mocked(fetch)
    .mock.calls.find((c) => String(c[0]).endsWith("/dashboard/mode"))!;
  expect(String((init as RequestInit).body)).toContain("mode=test");
});

test("Reopen closed tab appears once a tab was closed and restores it", async () => {
  await mount();
  act(() => api.openHref("/dashboard/apps", "new"));
  await screen.findByText("Apps body");
  act(() => api.close(api.state.activeId));
  await waitFor(() => expect(api.state.tabs).toHaveLength(1));
  ctrlK();
  fireEvent.click(
    await screen.findByRole("option", { name: /Reopen closed tab/ }),
  );
  await waitFor(() => expect(api.state.tabs).toHaveLength(2));
});

test("opened items are remembered and listed first next time", async () => {
  await mount();
  ctrlK();
  fireEvent.click(
    await screen.findByRole("option", { name: /Create payment link/ }),
  );
  await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());
  expect(localStorage.getItem("paymux-recent:u@example.com (admin)")).toContain(
    "Create payment link",
  );
  ctrlK();
  await box();
  expect(screen.getByText("Recent")).toBeTruthy();
  expect(optionNames()[0]).toContain("Create payment link");
});

test("free text offers payments and invoices searches", async () => {
  await mount();
  ctrlK();
  fireEvent.change(await box(), { target: { value: "alice" } });
  const names = optionNames();
  expect(names.some((n) => n.includes('payments for "alice"'))).toBe(true);
  expect(names.some((n) => n.includes('invoices for "alice"'))).toBe(true);
});

test("free text is never an empty list", async () => {
  await mount();
  ctrlK();
  fireEvent.change(await box(), { target: { value: "   " } });
  fireEvent.change(screen.getByRole("combobox"), {
    target: { value: "zzzzqq@@" },
  });
  // intents always offer a search, so the list is never empty for free text
  expect(screen.getAllByRole("option").length).toBeGreaterThan(0);
});
