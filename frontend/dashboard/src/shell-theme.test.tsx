import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import {
  handlers,
  link,
  mount,
  pageHtml,
  resetHost,
} from "./host.test.helpers";
import { clearThemeCache } from "./theme-apply";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
function manifest(name: string): string {
  const m = JSON.parse(
    readFileSync(resolve(root, `web/themes/${name}/theme.json`), "utf8"),
  );
  return JSON.stringify({
    name,
    scheme: m.dashboard.scheme ?? "",
    seeds: m.dashboard.seeds,
    dark_seeds: m.dashboard.dark_seeds ?? null,
    overrides: m.dashboard.overrides ?? {},
  });
}
const css = () =>
  document.querySelector("style#paymux-dashboard-theme")?.textContent ?? "";

beforeEach(() => {
  resetHost();
  clearThemeCache();
  document.head.innerHTML = "";
  document.documentElement.classList.remove("dark");
  vi.stubGlobal("matchMedia", (q: string) => ({
    matches: false,
    media: q,
    addEventListener() {},
    removeEventListener() {},
  }));
  for (const n of ["stripe", "paddle", "midnight"])
    handlers[`/theme/${n}/dashboard.json`] = () => manifest(n);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

test("a page that carries a theme paints it; switching pages switches --primary", async () => {
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", "<p>Overview body</p>", { dashboardTheme: "paddle" });
  handlers["/dashboard/payments"] = () =>
    pageHtml("Payments", "<p>Payments body</p>", { dashboardTheme: "stripe" });
  await mount();
  await waitFor(() => expect(css()).toContain("--primary:#0d7d68"));
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  await waitFor(() => expect(css()).toContain("--primary:#635bff"));
  expect(
    document.querySelectorAll("style#paymux-dashboard-theme"),
  ).toHaveLength(1);
});

test("a page without a theme wears stripe", async () => {
  await mount();
  await waitFor(() => expect(css()).toContain("--primary:#635bff"));
});

test("a failing manifest never breaks the page", async () => {
  delete handlers["/theme/stripe/dashboard.json"];
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", "<p>Overview body</p>", { dashboardTheme: "paddle" });
  delete handlers["/theme/paddle/dashboard.json"];
  await mount();
  await screen.findByText("Overview body");
  expect(css()).toBe("");
});

test("a dark scheme is the default until the user toggles", async () => {
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", "<p>Overview body</p>", {
      dashboardTheme: "midnight",
    });
  await mount();
  await waitFor(() =>
    expect(document.documentElement.classList.contains("dark")).toBe(true),
  );
});

test("the user's saved toggle beats the theme's scheme", async () => {
  localStorage.setItem("paymux-theme", "light");
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", "<p>Overview body</p>", {
      dashboardTheme: "midnight",
    });
  await mount();
  await waitFor(() => expect(css()).toContain("--primary:#4d7c0f"));
  expect(document.documentElement.classList.contains("dark")).toBe(false);
});

test("a light theme follows the system setting when nothing is saved", async () => {
  vi.stubGlobal("matchMedia", (q: string) => ({
    matches: true,
    media: q,
    addEventListener() {},
    removeEventListener() {},
  }));
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", "<p>Overview body</p>", { dashboardTheme: "paddle" });
  await mount();
  await waitFor(() => expect(css()).toContain("--primary:#0d7d68"));
  expect(document.documentElement.classList.contains("dark")).toBe(true);
});

test("editing the theme you wear shows at once, without a reload", async () => {
  let accent = "#0d7d68";
  handlers["/dashboard/overview"] = () =>
    pageHtml("Overview", "<p>Overview body</p>", {
      dashboardTheme: "paddle",
      themeVersion: "v1",
    });
  handlers["/dashboard/payments"] = () =>
    pageHtml("Payments", "<p>Payments body</p>", {
      dashboardTheme: "paddle",
      themeVersion: "v2",
    });
  handlers["/theme/paddle/dashboard.json"] = () =>
    JSON.stringify({
      ...JSON.parse(manifest("paddle")),
      seeds: { ...JSON.parse(manifest("paddle")).seeds, accent },
    });
  await mount();
  await waitFor(() => expect(css()).toContain("--primary:#0d7d68"));
  accent = "#b3261e"; // the theme was edited; the next page names a newer version
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  await waitFor(() => expect(css()).toContain("--primary:#b3261e"));
});
