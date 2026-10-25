import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";
import { contrast, derive } from "./theme-derive";

const here = dirname(fileURLToPath(import.meta.url));
const read = (rel: string) => readFileSync(resolve(here, rel), "utf8");
const work = read("./workspace.css");
const shell = read("./app-shell.css");

function rule(css: string, selector: string): string {
  const esc = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return css.match(new RegExp(`(?:^|\\n)${esc}\\s*\\{([^}]*)\\}`))?.[1] ?? "";
}

// The rule that remaps the page tokens for everything drawn on --sidebar:
// [selector list, body].
function surface(): [string, string] {
  for (const m of shell.matchAll(/(?:^|\n)([^{}@]+)\{([^}]*)\}/g))
    if (m[2].includes("--foreground: var(--sidebar-foreground)"))
      return [m[1], m[2]];
  return ["", ""];
}

test("sidebar text comes from --sidebar-foreground, not the page text tokens", () => {
  expect(rule(work, ".navigation-caption")).toContain(
    "color: var(--sidebar-foreground)",
  );
  expect(rule(work, ".navigation-item")).toContain(
    "color: var(--sidebar-foreground)",
  );
  expect(rule(work, ".navigation-item:hover")).toContain(
    "color: var(--sidebar-foreground)",
  );
  expect(rule(work, ".navigation-item.selected")).toContain(
    "color: var(--sidebar-foreground)",
  );
  expect(rule(shell, ".app-shell .navigation-item .nav-count")).toContain(
    "color: var(--sidebar-foreground)",
  );
});

test("everything drawn on the sidebar colour reads its text from the sidebar tokens", () => {
  const scope = surface()[1];
  expect(scope).toContain("--foreground: var(--sidebar-foreground)");
  expect(scope).toContain("--muted-foreground: var(--sidebar-foreground)");
  expect(scope).toMatch(
    /--muted: color-mix\(in srgb, var\(--sidebar-foreground\) \d+%, var\(--sidebar\)\)/,
  );
  expect(scope).toMatch(
    /--ui-accent: color-mix\(in srgb, var\(--primary\) \d+%, var\(--sidebar\)\)/,
  );
  // The page colours stay reachable for the islands inside (active tab, command box).
  expect(rule(shell, ".app-shell")).toContain(
    "--page-foreground: var(--foreground)",
  );
  expect(shell).toMatch(
    /\.app-shell \.tab\.active[^{]*\{[^}]*--foreground: var\(--page-foreground\)/,
  );
});

test("the sidebar surfaces are the sidebar, titlebar, tab strip and status bar", () => {
  const sel = surface()[0];
  for (const x of [
    ".react-sidebar",
    ".react-titlebar",
    ".tab-strip",
    ".status-bar",
  ])
    expect(sel, x).toContain(x);
});

// color-mix(in srgb, a p%, b) as the browser computes it.
function mixHex(a: string, pct: number, b: string): string {
  const pa = [1, 3, 5].map((i) => parseInt(a.slice(i, i + 2), 16));
  const pb = [1, 3, 5].map((i) => parseInt(b.slice(i, i + 2), 16));
  return (
    "#" +
    pa
      .map((v, i) => Math.round(v * (pct / 100) + pb[i] * (1 - pct / 100)))
      .map((v) => v.toString(16).padStart(2, "0"))
      .join("")
  );
}
const pct = (re: RegExp) => Number(shell.match(re)?.[1]);

test.each(["stripe", "paddle", "midnight"])(
  "%s: hovered and selected nav items stay readable on the sidebar",
  (name) => {
    const m = JSON.parse(
      read(`../../../web/themes/${name}/theme.json`),
    ).dashboard;
    const { light, dark } = derive(m.seeds, m.dark_seeds, m.overrides);
    const hover = pct(
      /--muted: color-mix\(in srgb, var\(--sidebar-foreground\) (\d+)%/,
    );
    const selected = pct(
      /--ui-accent: color-mix\(in srgb, var\(--primary\) (\d+)%/,
    );
    expect(hover).toBeGreaterThan(0);
    expect(selected).toBeGreaterThan(0);
    for (const [mode, t] of [
      ["light", light],
      ["dark", dark],
    ] as const) {
      const fg = t["--sidebar-foreground"];
      const bg = t["--sidebar"];
      expect(
        contrast(fg, mixHex(fg, hover, bg)),
        `${name} ${mode} hover`,
      ).toBeGreaterThanOrEqual(4.5);
      expect(
        contrast(fg, mixHex(t["--primary"], selected, bg)),
        `${name} ${mode} selected`,
      ).toBeGreaterThanOrEqual(4.5);
    }
  },
);
