import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
} from "@testing-library/react";
import { api, handlers, mount, pageHtml, resetHost } from "./host.test.helpers";

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const rows = ["pay_1", "pay_2", "pay_3"]
  .map(
    (id) =>
      `<tr><td><a href="/dashboard/payments/${id}">${id}</a></td><td>x</td></tr>`,
  )
  .join("");
const list = `<table id="t"><tbody>${rows}</tbody></table><table id="plain"><tbody><tr><td>no link</td></tr></tbody></table>`;

async function payments() {
  handlers["/dashboard/payments"] = () => pageHtml("Payments", list);
  handlers["/dashboard/payments/pay_2"] = () =>
    pageHtml("Payment", "<p>Payment pay_2 body</p>");
  return mount("/dashboard/payments");
}
const key = (
  code: string,
  init: KeyboardEventInit = {},
  target: Element = document.body,
) => fireEvent.keyDown(target, { code, key: code, ...init });
const focusedRow = () =>
  document.activeElement?.closest("tr")?.querySelector("a")?.textContent;

test("only tables whose rows contain a link are navigable", async () => {
  await payments();
  expect(document.getElementById("t")!.hasAttribute("data-nav")).toBe(true);
  expect(document.getElementById("plain")!.hasAttribute("data-nav")).toBe(
    false,
  );
});

test("j and k move the row focus and clamp at the ends", async () => {
  await payments();
  key("KeyJ");
  expect(focusedRow()).toBe("pay_1");
  key("KeyJ");
  key("KeyJ");
  key("KeyJ");
  expect(focusedRow()).toBe("pay_3");
  key("KeyK");
  expect(focusedRow()).toBe("pay_2");
  key("KeyK");
  key("KeyK");
  expect(focusedRow()).toBe("pay_1");
});

test("arrows, Home and End work once a row has focus", async () => {
  await payments();
  const before = fireEvent.keyDown(document.body, {
    code: "ArrowDown",
    key: "ArrowDown",
  });
  expect(before).toBe(true); // the page still scrolls
  expect(document.activeElement?.closest("tr")).toBeNull();
  key("KeyJ");
  key("ArrowDown");
  expect(focusedRow()).toBe("pay_2");
  key("End");
  expect(focusedRow()).toBe("pay_3");
  key("Home");
  expect(focusedRow()).toBe("pay_1");
  key("ArrowUp");
  expect(focusedRow()).toBe("pay_1");
});

test("the focused row is marked for a visible outline", async () => {
  await payments();
  key("KeyJ");
  const tr = document.activeElement as HTMLElement;
  expect(tr.tagName).toBe("TR");
  expect(tr.getAttribute("tabindex")).toBe("-1");
  expect(tr.hasAttribute("data-nav-row")).toBe(true);
});

test("Enter opens the row's first link in the current tab; Alt+Enter in a new tab", async () => {
  const router = await payments();
  key("KeyJ");
  key("KeyJ");
  key("Enter", {}, document.activeElement!);
  await screen.findByText("Payment pay_2 body");
  expect(router.state.location.pathname).toBe("/dashboard/payments/pay_2");
  expect(api.state.tabs).toHaveLength(2); // Overview, and payments replaced in place
});

test("Alt+Enter on a row opens a new tab", async () => {
  await payments();
  const before = api.state.tabs.length;
  key("KeyJ");
  key("Enter", { altKey: true }, document.activeElement!);
  await waitFor(() => expect(api.state.tabs.length).toBe(before + 1));
});

test("row keys are ignored while typing and for hidden tabs", async () => {
  await payments();
  const input = document.createElement("input");
  document.body.append(input);
  key("KeyJ", {}, input);
  expect(document.activeElement?.closest("tr")).toBeNull();
  input.remove();
  act(() => api.openHref("/dashboard/apps", "new"));
  await screen.findByText("Apps body");
  key("KeyJ");
  expect(document.querySelector("[data-nav-row]")).toBeNull();
});
