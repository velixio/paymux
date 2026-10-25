import type { ThemeSpec } from "./theme-derive";
import { writeStyle } from "./theme-style";

/** What GET /theme/<name>/dashboard.json serves. */
export type Manifest = ThemeSpec & { name: string; scheme: string };

const STYLE_ID = "paymux-dashboard-theme";
const FALLBACK = "stripe";
const HEX = /^#[0-9a-f]{6}$/i;

// One request per theme name; a failed one is forgotten so the next try asks again.
const manifests = new Map<string, Promise<Manifest | null>>();

async function fetchManifest(
  name: string,
  version?: string | null,
): Promise<Manifest | null> {
  try {
    const query = version ? `?v=${encodeURIComponent(version)}` : "";
    const res = await fetch(
      `/theme/${encodeURIComponent(name)}/dashboard.json${query}`,
      { credentials: "same-origin" },
    );
    if (!res.ok) return null;
    const m = JSON.parse(await res.text()) as Manifest;
    return HEX.test(m?.seeds?.accent ?? "") ? m : null;
  } catch {
    return null;
  }
}

// Keyed by name and version: a newer version of a theme is a new request, and
// the older ones of that name are dropped.
const keyOf = (name: string, version?: string | null) =>
  `${name}@${version ?? ""}`;

function loadManifest(
  name: string,
  version?: string | null,
): Promise<Manifest | null> {
  const key = keyOf(name, version);
  let p = manifests.get(key);
  if (!p) {
    for (const k of manifests.keys())
      if (k.startsWith(`${name}@`)) manifests.delete(k);
    p = fetchManifest(name, version);
    manifests.set(key, p);
    void p.then((m) => {
      if (!m && manifests.get(key) === p) manifests.delete(key);
    });
  }
  return p;
}

/** Forget fetched manifests (all, or one name), for example after a theme is edited. */
export function clearThemeCache(name?: string) {
  if (!name) return manifests.clear();
  for (const k of manifests.keys())
    if (k.startsWith(`${name}@`)) manifests.delete(k);
}

let latest = 0;

/**
 * Paints the named dashboard theme (stripe when name is null or its manifest
 * cannot be had). Resolves to the theme's default scheme, or null when nothing
 * could be loaded and the page keeps what it had. The latest call wins.
 */
export async function applyDashboardTheme(
  name: string | null,
  version?: string | null,
): Promise<{ scheme: string } | null> {
  const ticket = ++latest;
  const want = name || FALLBACK;
  // The derivation is a separate chunk; start loading it with the manifest.
  const derivation = import("./theme-derive");
  const m =
    (await loadManifest(want, version)) ??
    (want === FALLBACK ? null : await loadManifest(FALLBACK));
  const { themeCss } = await derivation;
  if (!m || ticket !== latest) return null;
  writeStyle(STYLE_ID, themeCss(m));
  return { scheme: m.scheme };
}
