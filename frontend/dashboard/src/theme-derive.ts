// Seeds -> the full dashboard token sets. Pure: the dashboard and the theme
// editor's live preview both use it. Status colours (--status-*) are never
// produced here; they keep their meaning in every theme.

export type Seeds = {
  accent: string;
  background?: string;
  sidebar?: string;
  text?: string;
  sidebar_text?: string;
};
export type Tokens = Record<string, string>;
export type Overrides = Partial<Record<"light" | "dark", Tokens>>;

/** Text pairs that must reach 4.5:1 (the non-status pairs of tokens.test.ts). */
export const TEXT_PAIRS: [string, string][] = [
  ["--foreground", "--background"],
  ["--muted-foreground", "--background"],
  ["--sidebar-foreground", "--sidebar"],
  ["--accent-foreground", "--ui-accent"],
  ["--primary-foreground", "--primary"],
];

export const MIN_CONTRAST = 4.5;

type RGB = [number, number, number];

const parse = (hex: string): RGB => {
  const n = parseInt(hex.slice(1), 16);
  return [n >> 16, (n >> 8) & 255, n & 255];
};
const toHex = (c: RGB) =>
  "#" +
  c
    .map((v) =>
      Math.round(Math.min(255, Math.max(0, v)))
        .toString(16)
        .padStart(2, "0"),
    )
    .join("");

function luminance(hex: string): number {
  const [r, g, b] = parse(hex).map((v) => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** WCAG contrast ratio of two #rrggbb colours (1..21). */
export function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

/** Mix a towards b by t (0..1), in sRGB. */
function mix(a: string, b: string, t: number): string {
  const [x, y] = [parse(a), parse(b)];
  return toHex(x.map((v, i) => v + (y[i] - v) * t) as RGB);
}

function toHsl(hex: string): [number, number, number] {
  const [r, g, b] = parse(hex).map((v) => v / 255);
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const l = (max + min) / 2;
  const d = max - min;
  if (d === 0) return [0, 0, l];
  const s = d / (1 - Math.abs(2 * l - 1));
  let h: number;
  if (max === r) h = ((g - b) / d + 6) % 6;
  else if (max === g) h = (b - r) / d + 2;
  else h = (r - g) / d + 4;
  return [h * 60, s, l];
}

function fromHsl(h: number, s: number, l: number): string {
  const c = (1 - Math.abs(2 * l - 1)) * s;
  const x = c * (1 - Math.abs(((h / 60) % 2) - 1));
  const m = l - c / 2;
  const [r, g, b] =
    h < 60
      ? [c, x, 0]
      : h < 120
        ? [x, c, 0]
        : h < 180
          ? [0, c, x]
          : h < 240
            ? [0, x, c]
            : h < 300
              ? [x, 0, c]
              : [c, 0, x];
  return toHex([(r + m) * 255, (g + m) * 255, (b + m) * 255]);
}

const minContrast = (fg: string, bgs: string[]) =>
  Math.min(...bgs.map((bg) => contrast(fg, bg)));

/**
 * Moves fg toward black or white (whichever reads better on the backgrounds)
 * in small steps until it reaches 4.5:1 on all of them. Colours that already
 * pass are returned unchanged.
 */
function nudge(fg: string, bgs: string[]): string {
  if (minContrast(fg, bgs) >= MIN_CONTRAST) return fg;
  const score = (end: string) => minContrast(end, bgs);
  const end = score("#000000") >= score("#ffffff") ? "#000000" : "#ffffff";
  for (let t = 0.02; t < 1; t += 0.02) {
    const c = mix(fg, end, t);
    if (minContrast(c, bgs) >= MIN_CONTRAST) return c;
  }
  /* v8 ignore next -- only if even pure black/white fails, which the callers rule out */
  return end;
}

const isLight = (hex: string) => luminance(hex) > 0.18;

/** Seeds for the dark set when the theme gives none: lightness inverted, hue kept. */
function autoDark(s: Seeds): Seeds {
  const light = fillSeeds(s);
  const [bh, bs] = toHsl(light.background);
  const [th, ts] = toHsl(light.text);
  const [h, sat] = bs > 0.05 ? [bh, bs] : [th, ts];
  const tint = Math.min(0.3, Math.max(0.1, sat));
  const [ah, as, al] = toHsl(s.accent);
  return {
    accent: isLight(s.accent) ? s.accent : fromHsl(ah, as, Math.max(al, 0.72)),
    background: fromHsl(h, tint, 0.08),
    sidebar: fromHsl(h, tint, 0.1),
    text: fromHsl(h, Math.min(0.25, tint), 0.92),
    sidebar_text: undefined,
  };
}

type Full = Required<Omit<Seeds, "sidebar_text">> & { sidebar_text?: string };

function fillSeeds(s: Seeds): Full {
  const background = s.background ?? "#ffffff";
  const text = s.text ?? (isLight(background) ? "#1a1f36" : "#e6e8ef");
  const sidebar = s.sidebar ?? mix(background, text, 0.03);
  return {
    accent: s.accent,
    background,
    sidebar,
    text,
    sidebar_text: s.sidebar_text,
  };
}

function deriveMode(s: Seeds, dark: boolean): Tokens {
  const { accent, background: bg, sidebar, text, sidebar_text } = fillSeeds(s);
  const fg = nudge(text, [bg]);
  // The muted surface is a faint tint of the page. On a mid-tone page neither
  // black nor white can read on both, so there the tint is dropped.
  const tint = mix(bg, fg, 0.03);
  let mutedFg = nudge(mix(fg, bg, 0.34), [bg, tint]);
  let muted = tint;
  if (minContrast(mutedFg, [bg, tint]) < MIN_CONTRAST) {
    muted = bg;
    mutedFg = nudge(mix(fg, bg, 0.34), [bg]);
  }
  const secondary = mix(bg, fg, 0.055);
  const uiAccent = mix(bg, accent, dark ? 0.2 : 0.12);

  const white = "#ffffff";
  const black = "#000000";
  const onPrimary =
    contrast(white, accent) >= contrast(black, accent) ? white : black;
  const sidebarText = nudge(sidebar_text ?? mix(fg, sidebar, 0.34), [sidebar]);

  return {
    "--background": bg,
    "--foreground": fg,
    "--card": bg,
    "--card-foreground": fg,
    "--popover": bg,
    "--popover-foreground": fg,
    "--primary": accent,
    "--primary-foreground": onPrimary,
    "--secondary": secondary,
    "--secondary-foreground": nudge(mix(fg, bg, dark ? 0 : 0.12), [secondary]),
    "--muted": muted,
    // Read against the page only: the sidebar text has its own token.
    "--muted-foreground": mutedFg,
    "--ui-accent": uiAccent,
    "--accent-foreground": nudge(mix(accent, fg, dark ? 0.5 : 0.15), [
      uiAccent,
    ]),
    "--destructive": nudge(dark ? "#fda29b" : "#b42318", [bg]),
    "--border": mix(bg, fg, 0.1),
    "--input": mix(bg, fg, 0.25),
    "--ring": accent,
    "--sidebar": sidebar,
    // Sidebar text is read against the sidebar only, so a dark sidebar can sit
    // on a light page. The accent as text on the selected item is 12% accent
    // over the sidebar.
    "--sidebar-accent-foreground": nudge(
      dark ? mix(accent, fg, 0.5) : mix(accent, sidebarText, 0.15),
      [mix(sidebar, accent, 0.12)],
    ),
    "--sidebar-foreground": sidebarText,
  };
}

const THEMEABLE = new Set(
  Object.keys(deriveMode({ accent: "#000000" }, false)),
);

/**
 * Builds the light and dark token sets. Dark seeds may be partial; missing
 * fields (or no dark seeds at all) come from the light seeds, lightness
 * inverted. Overrides are applied last, as given.
 */
export function derive(
  seeds: Seeds,
  darkSeeds?: Seeds | null,
  overrides?: Overrides | null,
): { light: Tokens; dark: Tokens } {
  const auto = autoDark(seeds);
  const definedDark = Object.fromEntries(
    Object.entries(darkSeeds ?? {}).filter(([, v]) => v),
  );
  const out = {
    light: deriveMode(seeds, false),
    dark: deriveMode({ ...auto, ...definedDark } as Seeds, true),
  };
  for (const mode of ["light", "dark"] as const) {
    for (const [k, v] of Object.entries(overrides?.[mode] ?? {}))
      if (THEMEABLE.has(k)) out[mode][k] = v;
    ensureReadable(out[mode]);
  }
  return out;
}

/**
 * Whatever the seeds and overrides say, text must read: each text token is
 * nudged (it only ever moves toward black or white) against the surface it is
 * drawn on, so a theme file, an import or a form without JavaScript cannot
 * produce unreadable text. Backgrounds are never changed here, except a muted
 * surface on a mid-tone page, which gives way to the page colour.
 */
function ensureReadable(t: Tokens) {
  const on = (fg: string, bgs: string[]) => {
    t[fg] = nudge(
      t[fg],
      bgs.map((b) => t[b]),
    );
  };
  on("--foreground", ["--background"]);
  on("--card-foreground", ["--card"]);
  on("--popover-foreground", ["--popover"]);
  on("--secondary-foreground", ["--secondary"]);
  on("--destructive", ["--background"]);
  on("--sidebar-foreground", ["--sidebar"]);
  on("--accent-foreground", ["--ui-accent"]);
  on("--primary-foreground", ["--primary"]);
  on("--muted-foreground", ["--background", "--muted"]);
  if (
    minContrast(t["--muted-foreground"], [t["--background"], t["--muted"]]) <
    MIN_CONTRAST
  ) {
    t["--muted"] = t["--background"];
    on("--muted-foreground", ["--background"]);
  }
  // Selected sidebar items: the accent over the sidebar, as the stylesheet draws it.
  t["--sidebar-accent-foreground"] = nudge(t["--sidebar-accent-foreground"], [
    mix(t["--sidebar"], t["--primary"], 0.12),
  ]);
}

export type ContrastRow = {
  pair: [string, string];
  ratio: number;
  ok: boolean;
};

/** The ratio of each text pair in tokens (pairs with a missing token are skipped). */
export function contrastReport(tokens: Tokens): ContrastRow[] {
  return TEXT_PAIRS.filter(([fg, bg]) => tokens[fg] && tokens[bg]).map(
    ([fg, bg]) => {
      const ratio = contrast(tokens[fg], tokens[bg]);
      return { pair: [fg, bg], ratio, ok: ratio >= MIN_CONTRAST };
    },
  );
}

/** What a stylesheet is made from: the "dashboard" block of a theme. */
export type ThemeSpec = {
  seeds: Seeds;
  dark_seeds?: Seeds | null;
  overrides?: Overrides | null;
};

const HEX = /^#[0-9a-f]{6}$/i;
const TOKEN = /^--[a-z][a-z-]*$/;

/** `selector{--token:#rrggbb;...}` for the themeable tokens in tokens. */
export function tokenBlock(selector: string, tokens: Tokens): string {
  const decls = Object.entries(tokens)
    .filter(
      ([k, v]) => TOKEN.test(k) && !k.startsWith("--status-") && HEX.test(v),
    )
    .map(([k, v]) => `${k}:${v};`)
    .join("");
  return `${selector}{${decls}}`;
}

/**
 * The stylesheet for a theme: light tokens on the root, dark tokens on .dark.
 * The selectors are one element more specific than the defaults in styles.css,
 * which the page links after this style, so the theme wins wherever it sits.
 */
export function themeCss(m: ThemeSpec): string {
  const { light, dark } = derive(m.seeds, m.dark_seeds, m.overrides);
  return tokenBlock("html:root", light) + tokenBlock("html.dark", dark);
}
