import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";
import {
  contrast,
  contrastReport,
  derive,
  type Overrides,
  type Seeds,
} from "./theme-derive";

// The happy-dom environment replaces URL, so resolve through the module path.
const root = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const read = (rel: string) => readFileSync(resolve(root, rel), "utf8");

type Manifest = {
  name: string;
  tokens?: unknown;
  font_stack?: unknown;
  dashboard: {
    seeds: Seeds;
    dark_seeds?: Seeds;
    scheme?: string;
    overrides?: Overrides;
  };
};
const load = (name: string): Manifest =>
  JSON.parse(read(`web/themes/${name}/theme.json`));

const names = ["stripe", "paddle", "midnight"];

function block(css: string, selector: string): Record<string, string> {
  const start = css.indexOf(`\n${selector} {`);
  const body = css.slice(start, css.indexOf("}", start));
  const out: Record<string, string> = {};
  for (const m of body.matchAll(/(--[\w-]+):\s*([^;]+);/g))
    out[m[1]] = m[2].trim();
  return out;
}

test.each(names)("%s passes every text pair in light and dark", (name) => {
  const m = load(name);
  expect(m.name).toBe(name);
  const { light, dark } = derive(
    m.dashboard.seeds,
    m.dashboard.dark_seeds,
    m.dashboard.overrides,
  );
  for (const [mode, t] of [
    ["light", light],
    ["dark", dark],
  ] as const) {
    const report = contrastReport(t);
    expect(report.length).toBe(5);
    for (const r of report)
      expect(
        r.ok,
        `${name} ${mode} ${r.pair.join(" on ")} = ${r.ratio.toFixed(2)}`,
      ).toBe(true);
  }
});

test.each(names)(
  "%s is dashboard-only: customer pages are untouched",
  (name) => {
    const m = load(name);
    expect(m.tokens).toBeUndefined();
    expect(m.font_stack).toBeUndefined();
  },
);

test("stripe reproduces today's styles.css palette exactly", () => {
  const css = read("frontend/styles.css");
  const m = load("stripe");
  const { light, dark } = derive(
    m.dashboard.seeds,
    m.dashboard.dark_seeds,
    m.dashboard.overrides,
  );
  const want = { light: block(css, ":root"), dark: block(css, ".dark") };
  for (const [mode, got] of [
    ["light", light],
    ["dark", dark],
  ] as const)
    for (const [k, v] of Object.entries(got))
      expect(v, `${mode} ${k}`).toBe(want[mode][k]);
  expect(m.dashboard.scheme ?? "").toBe("");
});

test("paddle keeps its dark sidebar in light mode, with light text on it", () => {
  const m = load("paddle");
  const { light, dark } = derive(
    m.dashboard.seeds,
    m.dashboard.dark_seeds,
    m.dashboard.overrides,
  );
  expect(light["--background"]).toBe("#f5f5f2");
  expect(light["--sidebar"]).toBe("#121417");
  expect(light["--primary"]).toBe("#0d7d68");
  expect(contrast(light["--sidebar-foreground"], "#ffffff")).toBeLessThan(3);
  expect(
    contrast(light["--sidebar-foreground"], light["--sidebar"]),
  ).toBeGreaterThanOrEqual(4.5);
  // Dark mode is its own set: a dark page.
  expect(contrast(dark["--background"], "#000000")).toBeLessThan(2);
});

test("midnight is dark by default and has a distinct light version", () => {
  const m = load("midnight");
  const { light, dark } = derive(
    m.dashboard.seeds,
    m.dashboard.dark_seeds,
    m.dashboard.overrides,
  );
  expect(m.dashboard.scheme).toBe("dark");
  expect(dark["--background"]).toBe("#0c0d10");
  expect(dark["--primary"]).toBe("#c6f36b");
  expect(contrast(light["--background"], "#ffffff")).toBeLessThan(1.3);
});
