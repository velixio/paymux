import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { setNonce } from "get-nonce";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  applyDashboardTheme,
  clearThemeCache,
  type Manifest,
} from "./theme-apply";
import { themeCss } from "./theme-derive";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
// What GET /theme/<name>/dashboard.json serves for a bundled theme.
function served(name: string): Manifest {
  const m = JSON.parse(
    readFileSync(resolve(root, `web/themes/${name}/theme.json`), "utf8"),
  );
  return {
    name,
    scheme: m.dashboard.scheme ?? "",
    seeds: m.dashboard.seeds,
    dark_seeds: m.dashboard.dark_seeds ?? null,
    overrides: m.dashboard.overrides ?? {},
  };
}

let fetched: string[];
let broken: Set<string>;
function stubFetch(delays: Record<string, number> = {}) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string) => {
      fetched.push(input);
      const name = input.split("/")[2];
      if (delays[name]) await new Promise((r) => setTimeout(r, delays[name]));
      if (broken.has(name))
        return { ok: false, status: 404, text: async () => "" };
      return {
        ok: true,
        status: 200,
        text: async () => JSON.stringify(served(name)),
      };
    }),
  );
}
const styles = () => document.querySelectorAll("style#paymux-dashboard-theme");

beforeEach(() => {
  fetched = [];
  broken = new Set();
  clearThemeCache();
  document.head.innerHTML = "";
  setNonce("n0nce");
  stubFetch();
});
afterEach(() => vi.unstubAllGlobals());

test("writes :root and .dark token blocks into one nonce'd style tag", async () => {
  const r = await applyDashboardTheme("paddle");
  expect(r).toEqual({ scheme: "" });
  expect(fetched).toEqual(["/theme/paddle/dashboard.json"]);
  expect(styles()).toHaveLength(1);
  const tag = styles()[0];
  expect(tag.getAttribute("nonce")).toBe("n0nce");
  const css = tag.textContent!;
  expect(css).toMatch(/^:root\{[^}]*--primary:#0d7d68;[^}]*\}\.dark\{[^}]*\}$/);
  expect(css).toContain("--sidebar:#121417");
  expect(css).not.toContain("--status-");
});

test("a theme switch replaces the style, never duplicating it", async () => {
  await applyDashboardTheme("paddle");
  await applyDashboardTheme("midnight");
  await applyDashboardTheme("stripe");
  expect(styles()).toHaveLength(1);
  expect(styles()[0].textContent).toContain("--primary:#635bff");
  expect(styles()[0].textContent).not.toContain("#0d7d68");
});

test("the manifest is fetched once per name", async () => {
  await applyDashboardTheme("paddle");
  await applyDashboardTheme("midnight");
  await applyDashboardTheme("paddle");
  expect(fetched).toEqual([
    "/theme/paddle/dashboard.json",
    "/theme/midnight/dashboard.json",
  ]);
  clearThemeCache("paddle");
  await applyDashboardTheme("paddle");
  expect(fetched).toHaveLength(3);
});

test("no theme name means stripe", async () => {
  await applyDashboardTheme(null);
  expect(fetched).toEqual(["/theme/stripe/dashboard.json"]);
  expect(styles()[0].textContent).toContain("--primary:#635bff");
});

test("scheme comes back so the shell can default to dark", async () => {
  expect(await applyDashboardTheme("midnight")).toEqual({ scheme: "dark" });
});

test("a missing manifest falls back to stripe, silently", async () => {
  broken.add("gone");
  const r = await applyDashboardTheme("gone");
  expect(r).toEqual({ scheme: "" });
  expect(styles()[0].textContent).toContain("--primary:#635bff");
  // A failed fetch is not cached: the next attempt asks again.
  broken.delete("gone");
  stubFetch();
  await applyDashboardTheme("gone");
  expect(fetched.filter((f) => f.includes("/gone/"))).toHaveLength(2);
});

test("a network error or bad JSON falls back too; nothing at all leaves the page as it was", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => {
      throw new Error("offline");
    }),
  );
  expect(await applyDashboardTheme("paddle")).toBeNull();
  expect(styles()).toHaveLength(0);
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({ ok: true, status: 200, text: async () => "{nope" })),
  );
  expect(await applyDashboardTheme("paddle")).toBeNull();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => JSON.stringify({ name: "x", seeds: { accent: "red" } }),
    })),
  );
  expect(await applyDashboardTheme("paddle")).toBeNull();
  expect(styles()).toHaveLength(0);
  for (const body of ["null", "{}", '{"seeds":{}}']) {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({ ok: true, status: 200, text: async () => body })),
    );
    expect(await applyDashboardTheme("paddle")).toBeNull();
  }
  // An applied theme stays when a later fetch fails.
  stubFetch();
  await applyDashboardTheme("midnight");
  broken.add("paddle");
  clearThemeCache();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => {
      throw new Error("offline");
    }),
  );
  expect(await applyDashboardTheme("paddle")).toBeNull();
  expect(styles()[0].textContent).toContain("--primary:#4d7c0f");
});

test("the latest request wins when answers arrive out of order", async () => {
  stubFetch({ paddle: 30 });
  const slow = applyDashboardTheme("paddle");
  const fast = applyDashboardTheme("midnight");
  await Promise.all([slow, fast]);
  expect(styles()).toHaveLength(1);
  expect(styles()[0].textContent).toContain("--primary:#4d7c0f");
});

test("themeCss only emits hex colours for allowed token names", () => {
  const m = served("stripe");
  m.overrides = {
    light: {
      "--border": "#112233",
      "--status-ok-bg": "#00ff00",
      "--x": "#000000",
    },
    dark: { "--ring": "url(x)" },
  };
  const css = themeCss(m);
  expect(css).toContain("--border:#112233");
  expect(css).not.toContain("--status-");
  expect(css).not.toContain("--x:");
  expect(css).not.toContain("url(");
});

test("no nonce set still writes the style", async () => {
  setNonce(undefined as unknown as string);
  await applyDashboardTheme("stripe");
  expect(styles()).toHaveLength(1);
  expect(styles()[0].hasAttribute("nonce")).toBe(false);
});
