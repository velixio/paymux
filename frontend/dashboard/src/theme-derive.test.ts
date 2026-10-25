import { readFileSync, readdirSync, existsSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";
import {
  contrast,
  contrastReport,
  derive,
  TEXT_PAIRS,
  type Seeds,
} from "./theme-derive";

// The happy-dom environment replaces URL, so resolve through the module path.
const themesRoot = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "../../../web/themes",
);

const stripeLight: Seeds = {
  accent: "#635bff",
  background: "#ffffff",
  sidebar: "#f7f8fa",
  text: "#1a1f36",
};
const stripeDark: Seeds = {
  accent: "#8b85ff",
  background: "#0f1117",
  sidebar: "#14161d",
  text: "#e6e8ef",
};

// Today's palette (frontend/styles.css), the reference for the Stripe seeds.
const todayLight: Record<string, string> = {
  "--background": "#ffffff",
  "--foreground": "#1a1f36",
  "--card": "#ffffff",
  "--card-foreground": "#1a1f36",
  "--popover": "#ffffff",
  "--popover-foreground": "#1a1f36",
  "--primary": "#635bff",
  "--primary-foreground": "#ffffff",
  "--secondary": "#f1f3f8",
  "--secondary-foreground": "#2a3050",
  "--muted": "#f7f8fa",
  "--muted-foreground": "#5c6478",
  "--ui-accent": "#ebeafe",
  "--accent-foreground": "#4b44d6",
  "--destructive": "#b42318",
  "--border": "#e3e8ee",
  "--input": "#c1c9d4",
  "--ring": "#635bff",
  "--sidebar": "#f7f8fa",
};
const todayDark: Record<string, string> = {
  "--background": "#0f1117",
  "--foreground": "#e6e8ef",
  "--card": "#14161d",
  "--card-foreground": "#e6e8ef",
  "--popover": "#14161d",
  "--popover-foreground": "#e6e8ef",
  "--primary": "#8b85ff",
  "--primary-foreground": "#0f1117",
  "--secondary": "#1d2029",
  "--secondary-foreground": "#e6e8ef",
  "--muted": "#1a1d26",
  "--muted-foreground": "#9aa1b2",
  "--ui-accent": "#24224a",
  "--accent-foreground": "#c4c1ff",
  "--destructive": "#fda29b",
  "--border": "#262a35",
  "--input": "#3d4354",
  "--ring": "#8b85ff",
  "--sidebar": "#14161d",
};

const rgb = (h: string) =>
  [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16));
const maxDiff = (a: string, b: string) =>
  Math.max(...rgb(a).map((v, i) => Math.abs(v - rgb(b)[i])));

const HEX = /^#[0-9a-f]{6}$/;

function expectPasses(label: string, tokens: Record<string, string>) {
  for (const r of contrastReport(tokens))
    expect(
      r.ok,
      `${label} ${r.pair.join(" on ")} = ${r.ratio.toFixed(2)}`,
    ).toBe(true);
}

test("contrast is the WCAG ratio", () => {
  expect(contrast("#000000", "#ffffff")).toBeCloseTo(21, 5);
  expect(contrast("#ffffff", "#ffffff")).toBeCloseTo(1, 5);
  expect(contrast("#ffffff", "#000000")).toBeCloseTo(21, 5);
});

test("the pair list mirrors tokens.test.ts and leaves status colours out", () => {
  expect(TEXT_PAIRS).toEqual([
    ["--foreground", "--background"],
    ["--muted-foreground", "--background"],
    ["--muted-foreground", "--sidebar"],
    ["--accent-foreground", "--ui-accent"],
    ["--primary-foreground", "--primary"],
  ]);
});

test("contrastReport lists each pair with its ratio and verdict", () => {
  const r = contrastReport({
    "--foreground": "#000000",
    "--background": "#ffffff",
    "--muted-foreground": "#cccccc",
  });
  const byPair = Object.fromEntries(r.map((x) => [x.pair.join("|"), x]));
  expect(byPair["--foreground|--background"].ok).toBe(true);
  expect(byPair["--foreground|--background"].ratio).toBeCloseTo(21, 3);
  expect(byPair["--muted-foreground|--background"].ok).toBe(false);
  // Pairs whose tokens are missing are skipped, not reported as failures.
  expect(r.length).toBe(2);
});

test("derive covers every non-status token and never emits --status-*", () => {
  const { light, dark } = derive(stripeLight, stripeDark);
  for (const mode of [light, dark]) {
    expect(Object.keys(mode).sort()).toEqual(Object.keys(todayLight).sort());
    for (const [k, v] of Object.entries(mode)) {
      expect(k.startsWith("--status-")).toBe(false);
      expect(v, k).toMatch(HEX);
    }
  }
});

test("Stripe seeds reproduce today's palette within a small tolerance", () => {
  const { light, dark } = derive(stripeLight, stripeDark);
  for (const [k, want] of Object.entries(todayLight))
    expect(
      maxDiff(light[k], want),
      `light ${k} ${light[k]} vs ${want}`,
    ).toBeLessThanOrEqual(24);
  for (const [k, want] of Object.entries(todayDark))
    expect(
      maxDiff(dark[k], want),
      `dark ${k} ${dark[k]} vs ${want}`,
    ).toBeLessThanOrEqual(24);
  // The seeds themselves come through exactly.
  expect(light["--background"]).toBe("#ffffff");
  expect(light["--sidebar"]).toBe("#f7f8fa");
  expect(light["--primary"]).toBe("#635bff");
  expect(dark["--background"]).toBe("#0f1117");
  expect(dark["--primary"]).toBe("#8b85ff");
});

test("light seeds alone give a dark set with a dark background", () => {
  const { light, dark } = derive(stripeLight);
  expect(light["--background"]).toBe("#ffffff");
  expectPasses("light", light);
  expectPasses("auto dark", dark);
  expect(contrast(dark["--background"], "#000000")).toBeLessThan(2); // dark
  expect(contrast(dark["--foreground"], dark["--background"])).toBeGreaterThan(
    10,
  );
});

test("partial dark seeds fall back to the derived dark values", () => {
  const { dark } = derive(stripeLight, { accent: "#ff0066" });
  expect(dark["--primary"]).toBe("#ff0066");
  expect(contrast(dark["--foreground"], dark["--background"])).toBeGreaterThan(
    10,
  );
});

const fixtures: [string, Seeds, Seeds | undefined][] = [
  ["stripe", stripeLight, stripeDark],
  ["stripe auto-dark", stripeLight, undefined],
  [
    "paddle",
    {
      accent: "#0a8f6b",
      background: "#fbfaf7",
      sidebar: "#f1efe9",
      text: "#23211c",
    },
    {
      accent: "#3ecf9e",
      background: "#16150f",
      sidebar: "#1d1c15",
      text: "#ece9df",
    },
  ],
  [
    "midnight",
    {
      accent: "#7c9cff",
      background: "#0b1020",
      sidebar: "#0f1730",
      text: "#e8ecf8",
    },
    undefined,
  ],
  [
    "yellow on white",
    { accent: "#ffeb3b", background: "#ffffff", text: "#ffff00" },
    undefined,
  ],
  [
    "black on black",
    {
      accent: "#000000",
      background: "#000000",
      sidebar: "#000000",
      text: "#000000",
    },
    {
      accent: "#000000",
      background: "#000000",
      sidebar: "#000000",
      text: "#000000",
    },
  ],
  [
    "white on white",
    {
      accent: "#ffffff",
      background: "#ffffff",
      sidebar: "#ffffff",
      text: "#ffffff",
    },
    undefined,
  ],
  [
    "mid grey",
    {
      accent: "#808080",
      background: "#808080",
      sidebar: "#7f7f7f",
      text: "#818181",
    },
    undefined,
  ],
  ["accent only", { accent: "#123456" }, undefined],
  [
    "dark background, no text seed",
    { accent: "#0b5e3a", background: "#000000" },
    undefined,
  ],
  ["green accent", { accent: "#0b5e3a" }, { accent: "#0b5e3a" }],
  ["purple accent", { accent: "#4b1a8f" }, undefined],
  ["magenta accent", { accent: "#8f1a5e" }, undefined],
  ["cyan accent", { accent: "#0b4f5e" }, undefined],
  ["orange accent", { accent: "#8f4b1a" }, undefined],
  [
    "sidebar text seed",
    {
      accent: "#635bff",
      background: "#ffffff",
      sidebar: "#1a1f36",
      sidebar_text: "#ffffff",
    },
    undefined,
  ],
];

test.each(fixtures)("%s passes every text pair after nudging", (name, l, d) => {
  const { light, dark } = derive(l, d);
  expectPasses(`${name} light`, light);
  expectPasses(`${name} dark`, dark);
  for (const mode of [light, dark]) {
    for (const [fg, bg] of [
      ["--card-foreground", "--card"],
      ["--popover-foreground", "--popover"],
      ["--secondary-foreground", "--secondary"],
      ["--muted-foreground", "--muted"],
      ["--destructive", "--background"],
    ])
      expect(
        contrast(mode[fg], mode[bg]),
        `${name} ${fg} on ${bg}`,
      ).toBeGreaterThanOrEqual(4.5);
  }
});

test("overrides win over derived values and are not nudged", () => {
  const { light, dark } = derive(stripeLight, stripeDark, {
    light: { "--border": "#123456", "--foreground": "#fefefe" },
    dark: { "--ring": "#abcdef" },
  });
  expect(light["--border"]).toBe("#123456");
  expect(light["--foreground"]).toBe("#fefefe");
  expect(dark["--ring"]).toBe("#abcdef");
  expect(dark["--border"]).not.toBe("#123456");
});

test("overrides cannot introduce status tokens or unknown names", () => {
  const { light } = derive(stripeLight, undefined, {
    light: { "--status-ok-bg": "#00ff00", "--nope": "#000000" },
  });
  expect(light["--status-ok-bg"]).toBeUndefined();
  expect(light["--nope"]).toBeUndefined();
});

test("every bundled theme with a dashboard block passes in light and dark", () => {
  const dir = `${themesRoot}/`;
  for (const name of readdirSync(dir)) {
    const f = `${dir}${name}/theme.json`;
    if (!existsSync(f)) continue;
    const m = JSON.parse(readFileSync(f, "utf8"));
    if (!m.dashboard) continue;
    const { light, dark } = derive(
      m.dashboard.seeds,
      m.dashboard.dark_seeds ?? undefined,
      m.dashboard.overrides,
    );
    expectPasses(`${name} light`, light);
    expectPasses(`${name} dark`, dark);
  }
});
