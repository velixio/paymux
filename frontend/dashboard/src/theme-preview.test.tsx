import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { handlers, mount, pageHtml, resetHost } from "./host.test.helpers";
import { derive } from "./theme-derive";
import ThemePreview, { parseSpec } from "./theme-preview";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const dash = (name: string) =>
  JSON.parse(
    readFileSync(resolve(root, `web/themes/${name}/theme.json`), "utf8"),
  ).dashboard;
const spec = (name: string) => {
  const d = dash(name);
  return JSON.stringify({
    seeds: d.seeds,
    dark_seeds: d.dark_seeds ?? null,
    overrides: d.overrides ?? {},
  });
};
const rgb = (hex: string) => {
  const n = parseInt(hex.slice(1), 16);
  return `rgb(${n >> 16}, ${(n >> 8) & 255}, ${n & 255})`;
};

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

test("parseSpec accepts a spec and rejects anything else", () => {
  expect(parseSpec(spec("paddle"))?.seeds.accent).toBe("#0d7d68");
  expect(parseSpec(undefined)).toBeNull();
  expect(parseSpec("{nope")).toBeNull();
  expect(parseSpec("null")).toBeNull();
  expect(parseSpec(JSON.stringify({ seeds: { accent: "red" } }))).toBeNull();
});

test("shows the theme in light and dark, painted with the derived tokens", () => {
  const d = dash("paddle");
  const { light, dark } = derive(d.seeds, d.dark_seeds, d.overrides);
  render(<ThemePreview spec={parseSpec(spec("paddle"))!} name="paddle" />);
  const [l, k] = screen.getAllByRole("img");
  expect(l.getAttribute("aria-label")).toBe("paddle, light");
  expect(k.getAttribute("aria-label")).toBe("paddle, dark");
  expect(l.style.background).toBe(rgb(light["--background"]));
  expect(k.style.background).toBe(rgb(dark["--background"]));
  const side = (el: HTMLElement) =>
    el.querySelector<HTMLElement>("[data-part=sidebar]")!.style.background;
  // Paddle keeps a dark sidebar in light mode.
  expect(side(l)).toBe(rgb("#121417"));
  const btn = (el: HTMLElement) =>
    el.querySelector<HTMLElement>("[data-part=button]")!;
  expect(btn(l).style.background).toBe(rgb(light["--primary"]));
  expect(btn(l).style.color).toBe(rgb(light["--primary-foreground"]));
});

test("a card on the Appearance page gets its preview, and no-JS markup stays plain", async () => {
  handlers["/dashboard/appearance"] = () =>
    pageHtml(
      "Appearance",
      `<h1>Appearance</h1><ul class="theme-cards"><li class="theme-card" data-theme-card="paddle"><div class="theme-preview" data-theme-preview data-theme-spec='${spec("paddle")}'></div><h4>paddle</h4></li><li class="theme-card" data-theme-card="pink"><h4>pink</h4></li></ul>`,
    );
  await mount("/dashboard/appearance");
  await waitFor(() => expect(screen.getAllByRole("img")).toHaveLength(2));
  expect(screen.getByText("pink")).toBeTruthy();
});

test("a broken spec renders no preview and does not crash the page", async () => {
  handlers["/dashboard/appearance"] = () =>
    pageHtml(
      "Appearance",
      `<h1>Appearance</h1><div class="theme-preview" data-theme-preview data-theme-spec="{bad"></div><p>still here</p>`,
    );
  await mount("/dashboard/appearance");
  await screen.findByText("still here");
  expect(screen.queryAllByRole("img")).toHaveLength(0);
});
