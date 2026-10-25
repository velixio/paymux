import { getNonce } from "get-nonce";
import type { ThemeSpec } from "./theme-derive";

/** What GET /theme/<name>/dashboard.json serves. */
export type Manifest = ThemeSpec & { name: string; scheme: string };

const STYLE_ID = "paymux-dashboard-theme";
const FALLBACK = "stripe";
const HEX = /^#[0-9a-f]{6}$/i;

// One request per theme name; a failed one is forgotten so the next try asks again.
const manifests = new Map<string, Promise<Manifest | null>>();

async function fetchManifest(name: string): Promise<Manifest | null> {
  try {
    const res = await fetch(
      `/theme/${encodeURIComponent(name)}/dashboard.json`,
      {
        credentials: "same-origin",
      },
    );
    if (!res.ok) return null;
    const m = JSON.parse(await res.text()) as Manifest;
    return HEX.test(m?.seeds?.accent ?? "") ? m : null;
  } catch {
    return null;
  }
}

function loadManifest(name: string): Promise<Manifest | null> {
  let p = manifests.get(name);
  if (!p) {
    p = fetchManifest(name);
    manifests.set(name, p);
    void p.then((m) => {
      if (!m && manifests.get(name) === p) manifests.delete(name);
    });
  }
  return p;
}

/** Forget fetched manifests (all, or one name), for example after a theme is edited. */
export function clearThemeCache(name?: string) {
  if (name) manifests.delete(name);
  else manifests.clear();
}

let latest = 0;

/**
 * Paints the named dashboard theme (stripe when name is null or its manifest
 * cannot be had). Resolves to the theme's default scheme, or null when nothing
 * could be loaded and the page keeps what it had. The latest call wins.
 */
export async function applyDashboardTheme(
  name: string | null,
): Promise<{ scheme: string } | null> {
  const ticket = ++latest;
  const want = name || FALLBACK;
  // The derivation is a separate chunk; start loading it with the manifest.
  const derivation = import("./theme-derive");
  const m =
    (await loadManifest(want)) ??
    (want === FALLBACK ? null : await loadManifest(FALLBACK));
  const { themeCss } = await derivation;
  if (!m || ticket !== latest) return null;
  let tag = document.getElementById(STYLE_ID);
  if (!tag) {
    tag = document.createElement("style");
    tag.id = STYLE_ID;
    document.head.append(tag);
  }
  const nonce = getNonce();
  if (nonce) tag.setAttribute("nonce", nonce);
  tag.textContent = themeCss(m);
  return { scheme: m.scheme };
}
