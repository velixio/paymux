import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { mount, resetHost } from "./host.test.helpers";

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const question = (target: Element = document.body) =>
  fireEvent.keyDown(target, { code: "Slash", key: "?", shiftKey: true });

test("? opens the shortcut sheet listing every binding", async () => {
  await mount();
  question();
  const sheet = await screen.findByRole("dialog", {
    name: "Keyboard shortcuts",
  });
  const text = sheet.textContent ?? "";
  for (const want of [
    "Previous tab",
    "Next tab",
    "Go to tab 1 to 9",
    "Close tab",
    "Reopen closed tab",
    "Open command bar",
    "Next row",
    "Previous row",
    "First row",
    "Last row",
    "Open row",
    "Open row in a new tab",
    "Show shortcuts",
  ])
    expect(text).toContain(want);
  expect(within(sheet).getAllByText("[").length).toBeGreaterThan(0);
  fireEvent.keyDown(sheet, { key: "Escape", code: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

test("? is ignored while typing", async () => {
  await mount();
  const input = document.createElement("input");
  document.body.append(input);
  question(input);
  expect(screen.queryByRole("dialog")).toBeNull();
  input.remove();
});

test("the footer button opens the sheet too", async () => {
  await mount();
  fireEvent.click(screen.getByRole("button", { name: "Keyboard shortcuts" }));
  await screen.findByRole("dialog", { name: "Keyboard shortcuts" });
});
