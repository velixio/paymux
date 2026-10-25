import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
} from "@testing-library/react";
import {
  handlers,
  link,
  mount,
  pageHtml,
  resetHost,
} from "./host.test.helpers";

const here = dirname(fileURLToPath(import.meta.url));

function editor(overrides = "{}") {
  return `<h1>Edit mine</h1><div data-theme-editor data-overrides='${overrides}'><form method="post" action="/dashboard/appearance/themes/mine"><input type="hidden" name="csrf" value="t"><input name="version" value="1.0.0"><input type="color" name="light_accent" value="#0d7d68"><input type="color" name="light_background" value="#f5f5f2"><input type="checkbox" name="set_light_background" value="1" checked><input type="color" name="light_text" value="#16181d"><input type="checkbox" name="set_light_text" value="1"><input type="checkbox" name="dark_enabled" value="1"><input type="color" name="dark_accent" value="#34c9a9"><input type="color" name="dark_background" value="#14161a"><input type="checkbox" name="set_dark_background" value="1" checked><select name="scheme"><option value="" selected>Follow</option><option value="dark">Dark</option></select><button type="submit">Save theme</button></form></div>`;
}
const preview = () =>
  document.querySelector("style#paymux-theme-preview")?.textContent ?? "";
const input = (name: string) =>
  document.querySelector<HTMLInputElement>(`input[name=${name}]`)!;

beforeEach(() => {
  resetHost();
  document.head.innerHTML = "";
  document.documentElement.classList.remove("dark");
  vi.stubGlobal("matchMedia", (q: string) => ({
    matches: false,
    media: q,
    addEventListener() {},
    removeEventListener() {},
  }));
  handlers["/dashboard/appearance/themes/mine/edit"] = () =>
    pageHtml("Edit mine", editor());
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

test("the editor is a lazy chunk: render.tsx only imports it dynamically", () => {
  const src = readFileSync(resolve(here, "render.tsx"), "utf8");
  expect(src).not.toMatch(/from "\.\/theme-editor"/);
  expect(src).toMatch(/import\("\.\/theme-editor"\)/);
});

test("the form works before (and without) the editor", async () => {
  await mount("/dashboard/appearance/themes/mine/edit");
  expect(screen.getByRole("button", { name: "Save theme" })).toBeTruthy();
  expect(input("light_accent").value).toBe("#0d7d68");
});

test("the form's inputs are not re-created when the editor arrives", async () => {
  await mount("/dashboard/appearance/themes/mine/edit");
  const version = input("version");
  version.value = "9.9.9"; // typed before the editor chunk is ready
  await waitFor(() => expect(preview()).not.toBe(""));
  expect(input("version")).toBe(version);
  expect(input("version").value).toBe("9.9.9");
});

test("previews the theme live on the whole dashboard, and shows contrast", async () => {
  await mount("/dashboard/appearance/themes/mine/edit");
  await waitFor(() => expect(preview()).toContain("--primary:#0d7d68"));
  // The contrast table lists each text pair for light and dark with a verdict.
  const table = await screen.findByRole("table", { name: /contrast/i });
  expect(table.textContent).toContain("✓");
  expect(table.textContent).not.toContain("✗");

  fireEvent.change(input("light_accent"), { target: { value: "#ff0066" } });
  await waitFor(() => expect(preview()).toContain("--primary:#ff0066"));
  // A seed whose box is unchecked is not used.
  fireEvent.change(input("light_text"), { target: { value: "#222222" } });
  await waitFor(() => expect(preview()).toContain("--primary:#ff0066"));
  expect(preview()).not.toContain("#222222");
  fireEvent.click(input("set_light_text"));
  await waitFor(() => expect(preview()).toContain("--foreground:#222222"));
});

test("switches the preview between light and dark", async () => {
  await mount("/dashboard/appearance/themes/mine/edit");
  await waitFor(() => expect(preview()).toContain("--background:#f5f5f2"));
  fireEvent.click(screen.getByRole("button", { name: "Preview dark" }));
  // Until "choose myself" is on, the dark look is worked out from the light seeds.
  await waitFor(() => expect(preview()).not.toContain("--background:#f5f5f2"));
  expect(preview()).not.toContain("#14161a");
  fireEvent.click(input("dark_enabled"));
  await waitFor(() => expect(preview()).toContain("--primary:#34c9a9"));
  expect(preview()).toContain("--background:#14161a");
  fireEvent.click(screen.getByRole("button", { name: "Preview light" }));
  await waitFor(() => expect(preview()).toContain("--background:#f5f5f2"));
});

test("the page's own tokens come back when the editor is left", async () => {
  await mount("/dashboard/appearance/themes/mine/edit");
  await waitFor(() => expect(preview()).not.toBe(""));
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  await waitFor(() =>
    expect(document.querySelector("style#paymux-theme-preview")).toBeNull(),
  );
});

test("the preview is lifted while the editor's tab is hidden, and back when shown", async () => {
  await mount("/dashboard/appearance/themes/mine/edit");
  await waitFor(() => expect(preview()).toContain("--primary:#0d7d68"));
  // Alt-click opens the page in another tab; the editor's tab stays open, hidden.
  fireEvent.click(link("Payments"), { altKey: true });
  await screen.findByText("Payments body");
  await waitFor(() =>
    expect(document.querySelector("style#paymux-theme-preview")).toBeNull(),
  );
  expect(document.querySelector("form")).not.toBeNull(); // still mounted, unsaved work kept
  fireEvent.click(screen.getByRole("tab", { name: /Edit mine/ }));
  await waitFor(() => expect(preview()).toContain("--primary:#0d7d68"));
});

test("a theme whose colours cannot be read cannot be saved", async () => {
  handlers["/dashboard/appearance/themes/mine/edit"] = () =>
    pageHtml("Edit mine", editor('{"light":{"--foreground":"#f5f5f2"}}'));
  await mount("/dashboard/appearance/themes/mine/edit");
  const save = await screen.findByRole("button", { name: "Save theme" });
  await waitFor(() => expect((save as HTMLButtonElement).disabled).toBe(true));
  const table = await screen.findByRole("table", { name: /contrast/i });
  expect(table.textContent).toContain("✗");
  expect(screen.getByRole("alert").textContent).toMatch(/hard to read/i);
});

test("a colour the browser could not parse is ignored, not painted", async () => {
  await mount("/dashboard/appearance/themes/mine/edit");
  await waitFor(() => expect(preview()).toContain("--primary:#0d7d68"));
  // (A colour input would sanitise this itself; a text input shows the guard.)
  input("light_accent").type = "text";
  fireEvent.change(input("light_accent"), { target: { value: "nope" } });
  await new Promise((r) => setTimeout(r, 20));
  expect(preview()).toContain("--primary:#0d7d68");
});

test("leaving with unsaved changes asks first; saving does not", async () => {
  await mount("/dashboard/appearance/themes/mine/edit");
  await waitFor(() => expect(preview()).not.toBe(""));
  const leave = () => {
    const ev = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(ev);
    return ev.defaultPrevented;
  };
  expect(leave()).toBe(false);
  fireEvent.change(input("light_accent"), { target: { value: "#ff0066" } });
  await waitFor(() => expect(preview()).toContain("#ff0066"));
  expect(leave()).toBe(true);
  // Submitting is leaving on purpose.
  const form = document.querySelector("form")!;
  // (Keep the runtime's own submit handler out of this: it would post the form.)
  form.addEventListener("submit", (e) => e.stopPropagation());
  act(() => {
    form.dispatchEvent(
      new Event("submit", { bubbles: true, cancelable: true }),
    );
  });
  expect(leave()).toBe(false);
});

test.each(["{bad", "null", "7"])(
  "an unreadable overrides attribute (%s) is treated as none",
  async (raw) => {
    handlers["/dashboard/appearance/themes/mine/edit"] = () =>
      pageHtml("Edit mine", editor(raw));
    await mount("/dashboard/appearance/themes/mine/edit");
    await waitFor(() => expect(preview()).toContain("--primary:#0d7d68"));
  },
);
