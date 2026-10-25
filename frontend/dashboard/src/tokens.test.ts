import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

const read = (rel: string) =>
  readFileSync(fileURLToPath(new URL(rel, import.meta.url)), "utf8");

const css = read("../../styles.css");

function block(selector: string): Record<string, string> {
  const start = css.indexOf(`\n${selector} {`);
  const body = css.slice(start, css.indexOf("}", start));
  const out: Record<string, string> = {};
  for (const m of body.matchAll(/(--[\w-]+):\s*([^;]+);/g))
    out[m[1]] = m[2].trim();
  return out;
}

const light = block(":root");
const dark = block(".dark");

function luminance(hex: string): number {
  const n = parseInt(hex.slice(1), 16);
  const [r, g, b] = [n >> 16, (n >> 8) & 255, n & 255].map((v) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

const keys = [
  "--background",
  "--sidebar",
  "--foreground",
  "--muted-foreground",
  "--border",
  "--primary",
  "--ui-accent",
  "--accent-foreground",
];

test("light tokens match spec", () => {
  const want = [
    "#ffffff",
    "#f7f8fa",
    "#1a1f36",
    "#5c6478",
    "#e3e8ee",
    "#635bff",
    "#ebeafe",
    "#4b44d6",
  ];
  expect(keys.map((k) => light[k])).toEqual(want);
});

test("dark tokens match spec", () => {
  const want = [
    "#0f1117",
    "#14161d",
    "#e6e8ef",
    "#9aa1b2",
    "#262a35",
    "#8b85ff",
    "#24224a",
    "#c4c1ff",
  ];
  expect(keys.map((k) => dark[k])).toEqual(want);
});

test("text pairs reach 4.5:1 in both modes", () => {
  for (const [mode, t] of [
    ["light", light],
    ["dark", dark],
  ] as const) {
    const pairs: [string, string][] = [
      ["--foreground", "--background"],
      ["--muted-foreground", "--background"],
      ["--muted-foreground", "--sidebar"],
      ["--accent-foreground", "--ui-accent"],
      ["--primary-foreground", "--primary"],
      ...["ok", "warn", "bad", "neutral"].map(
        (s) => [`--status-${s}-fg`, `--status-${s}-bg`] as [string, string],
      ),
    ];
    for (const [fg, bg] of pairs) {
      expect(t[fg], `${mode} ${fg}`).toBeDefined();
      expect(t[bg], `${mode} ${bg}`).toBeDefined();
      expect(
        contrast(t[fg], t[bg]),
        `${mode} ${fg} on ${bg}`,
      ).toBeGreaterThanOrEqual(4.5);
    }
  }
  expect(light["--test-mode"]).toBe("#f59e0b");
});

test("customer pages keep their palette", () => {
  for (const f of [
    "../../../web/static/css/app.css",
    "../../../web/themes/default/theme.json",
  ]) {
    expect(read(f).toLowerCase()).not.toContain("635bff");
  }
});

test("link buttons use the on-primary text colour, not the link colour", () => {
  const shell = read("./app-shell.css");
  const work = read("./workspace.css");
  // The generic in-app link colour must not apply to a.btn ...
  expect(work).toMatch(/a:not\(\.navigation-item\)[^{]*:not\(\.btn\)/);
  // ... and a.btn sets its own text and background from the tokens.
  const rule = shell.match(/\.paymux-react a\.btn\s*\{([^}]*)\}/)?.[1] ?? "";
  expect(rule).toContain("color: var(--primary-foreground)");
  expect(rule).toContain("background: var(--primary)");
});

test("warning strip and mode labels take their colours from the tokens", () => {
  const work = read("./workspace.css");
  const rule = (selector: string) =>
    work.match(
      new RegExp(`\\n${selector.replace(".", "\\.")}\\s*\\{([^}]*)\\}`),
    )?.[1] ?? "";
  for (const selector of [".workspace-warning", ".live-label", ".test-label"]) {
    // A fixed hex colour would ignore the palette and dark mode.
    expect(rule(selector), selector).not.toMatch(/#[0-9a-f]{3,8}\b/i);
  }
  expect(rule(".workspace-warning")).toContain("var(--status-warn-bg)");
  expect(rule(".live-label")).toContain("var(--status-ok-bg)");
});
