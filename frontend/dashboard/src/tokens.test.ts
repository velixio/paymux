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

test("warning rows (status-bar popover) and mode labels take their colours from the tokens", () => {
  const work = read("./workspace.css");
  const rule = (selector: string) =>
    work.match(
      new RegExp(`\\n${selector.replace(".", "\\.")}\\s*\\{([^}]*)\\}`),
    )?.[1] ?? "";
  for (const selector of [".workspace-warning", ".live-label", ".test-label"]) {
    // A fixed hex colour would ignore the palette and dark mode.
    expect(rule(selector), selector).not.toMatch(/#[0-9a-f]{3,8}\b/i);
  }
  // The popover rows sit on the page like the rest of the shell; only their icon
  // carries the warning colour.
  expect(rule(".workspace-warning")).toContain("background: var(--card)");
  expect(rule(".workspace-warning svg")).toContain(
    "color: var(--status-warn-fg)",
  );
  expect(rule(".live-label")).toContain("var(--status-ok-bg)");
});

test("server-rendered banners match the in-app warning strip", () => {
  // The server page shows until React mounts (and on sign-in), so a reload
  // must not flash a different banner.
  const dash = read("../../../web/static/css/dash.css");
  const rules = [
    ...dash
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .matchAll(/(?:^|\n)([^{}]+)\{([^}]*)\}/g),
  ];
  for (const selector of [".dev-banner", ".banner-warning"]) {
    // The top-level rule whose selector list names exactly this class.
    const rule =
      rules.find(([, sel]) =>
        sel
          .split(",")
          .map((s) => s.trim())
          .includes(selector),
      )?.[2] ?? "";
    expect(rule, selector).toContain("background: var(--surface)");
    expect(rule, selector).not.toMatch(/#[0-9a-f]{3,8}\b/i);
  }
});

test("the server page stays hidden while the workspace loads, with a fallback", () => {
  const dash = read("../../../web/static/css/dash.css");
  const rule =
    dash.match(
      /#paymux-react-root \+ #paymux-server-page\s*\{([^}]*)\}/,
    )?.[1] ?? "";
  expect(rule).toContain("visibility: hidden");
  // Shown anyway after 2 s when the script is blocked or slow.
  expect(rule).toMatch(/animation: paymux-server-reveal 0s 2s forwards/);
  expect(dash).toMatch(
    /@keyframes paymux-server-reveal\s*\{\s*to\s*\{\s*visibility: visible;/,
  );
});

test("dashboard stylesheets use no fixed colours outside the tokens", () => {
  for (const f of ["./workspace.css", "./app-shell.css"]) {
    const fixed = read(f)
      .split("\n")
      .filter((line) => !line.trim().startsWith("--"))
      .filter((line) => /#[0-9a-f]{6}\b/i.test(line));
    expect(fixed, f).toEqual([]);
  }
});

test("loading placeholders are neutral and wait 150 ms before showing", () => {
  // A fast load never paints a placeholder, so reloads do not blink.
  expect(read("../../components/ui/skeleton.tsx")).toContain("bg-muted");
  expect(read("../../components/ui/skeleton.tsx")).not.toContain("bg-accent");
  const work = read("./workspace.css");
  const rule =
    work.match(
      /\.workspace-loading,\s*\.revenue-chart-loading\s*\{([^}]*)\}/,
    )?.[1] ?? "";
  expect(rule).toMatch(/animation: paymux-delayed-show 0s 150ms both/);
  expect(work).toMatch(
    /@keyframes paymux-delayed-show\s*\{\s*from\s*\{\s*opacity: 0;/,
  );
});

test("the filter row is borderless and compact", () => {
  const shell = read("./app-shell.css");
  const rule =
    shell.match(
      /\.app-shell \.react-content form\.filters\s*\{([^}]*)\}/,
    )?.[1] ?? "";
  expect(rule).toContain("border: 0");
  expect(rule).toContain("background: none");
  expect(rule).not.toMatch(/#[0-9a-f]{3,8}\b/i);
});

test("CSS of the removed header, footer and brand card is gone", () => {
  const css = read("./workspace.css") + read("./app-shell.css");
  for (const dead of [
    ".react-header",
    ".header-location",
    ".workspace-bottom",
    ".workspace-identity",
    ".workspace-initial",
    ".sidebar-bottom",
    ".ownership-dot",
    ".breadcrumb-root",
    ".active-marker",
  ])
    expect(css, dead).not.toContain(dead);
});
