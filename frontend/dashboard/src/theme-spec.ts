import type { ThemeSpec } from "./theme-derive";

const HEX = /^#[0-9a-f]{6}$/i;

/** Reads the spec a server-rendered card carries; null when it is not usable. */
export function parseSpec(raw: string | undefined): ThemeSpec | null {
  if (!raw) return null;
  try {
    const spec = JSON.parse(raw) as ThemeSpec | null;
    return HEX.test(spec?.seeds?.accent ?? "") ? spec : null;
  } catch {
    return null;
  }
}
