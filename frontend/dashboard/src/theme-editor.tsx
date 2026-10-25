import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { getNonce } from "get-nonce";
import {
  contrastReport,
  derive,
  tokenBlock,
  type Overrides,
  type Seeds,
  type ThemeSpec,
} from "./theme-derive";

const STYLE_ID = "paymux-theme-preview";
const HEX = /^#[0-9a-f]{6}$/i;
const PAIR_LABELS: Record<string, string> = {
  "--foreground|--background": "Text on the page",
  "--muted-foreground|--background": "Muted text on the page",
  "--sidebar-foreground|--sidebar": "Sidebar text on the sidebar",
  "--accent-foreground|--ui-accent": "Highlighted text on its tint",
  "--primary-foreground|--primary": "Button text on a button",
};

type Mode = "light" | "dark";

function field(form: HTMLFormElement, name: string): HTMLInputElement | null {
  return form.elements.namedItem(name) as HTMLInputElement | null;
}

function readSeeds(form: HTMLFormElement, prefix: string): Seeds | null {
  const accent = field(form, `${prefix}_accent`)?.value ?? "";
  if (!HEX.test(accent)) return null;
  const seeds: Seeds = { accent };
  for (const [key, out] of [
    ["background", "background"],
    ["sidebar", "sidebar"],
    ["text", "text"],
    ["sidebar_text", "sidebar_text"],
  ] as const) {
    const value = field(form, `${prefix}_${key}`)?.value ?? "";
    if (field(form, `set_${prefix}_${key}`)?.checked && HEX.test(value))
      seeds[out] = value;
  }
  return seeds;
}

/** What the form currently says, or null while the accent is not a colour. */
function readSpec(
  form: HTMLFormElement,
  overrides: Overrides,
): ThemeSpec | null {
  const seeds = readSeeds(form, "light");
  if (!seeds) return null;
  const dark = field(form, "dark_enabled")?.checked
    ? readSeeds(form, "dark")
    : null;
  return { seeds, dark_seeds: dark, overrides };
}

function parseOverrides(raw: string | undefined): Overrides {
  try {
    const o = JSON.parse(raw || "{}");
    return o && typeof o === "object" ? (o as Overrides) : {};
  } catch {
    return {};
  }
}

/**
 * Enhances the server-rendered theme form: while you edit, the whole dashboard
 * wears the theme you are making (restored when you leave), the contrast of
 * every text pair is listed, and a theme with unreadable text cannot be saved.
 * Without this the form is a plain form that works on its own.
 */
export default function ThemeEditor({
  overrides: rawOverrides,
  children,
}: {
  overrides?: string;
  children: ReactNode;
}) {
  const root = useRef<HTMLDivElement>(null);
  const dirty = useRef(false);
  const overrides = useMemo(() => parseOverrides(rawOverrides), [rawOverrides]);
  const [spec, setSpec] = useState<ThemeSpec | null>(null);
  const [mode, setMode] = useState<Mode>(() =>
    document.documentElement.classList.contains("dark") ? "dark" : "light",
  );

  // Read the form now and whenever it changes.
  useEffect(() => {
    const el = root.current;
    const form = el?.querySelector("form");
    if (!el || !form) return;
    const refresh = (e?: Event) => {
      if (e && (e.target as HTMLElement).closest("input,select"))
        dirty.current = true;
      const next = readSpec(form, overrides);
      if (next) setSpec(next);
    };
    refresh();
    for (const type of ["input", "change", "click"])
      el.addEventListener(type, refresh);
    const submitted = () => {
      dirty.current = false;
    };
    form.addEventListener("submit", submitted);
    const guard = (e: BeforeUnloadEvent) => {
      if (!dirty.current) return;
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", guard);
    return () => {
      for (const type of ["input", "change", "click"])
        el.removeEventListener(type, refresh);
      form.removeEventListener("submit", submitted);
      window.removeEventListener("beforeunload", guard);
    };
  }, [overrides]);

  const derived = useMemo(
    () => (spec ? derive(spec.seeds, spec.dark_seeds, spec.overrides) : null),
    [spec],
  );
  const reports = useMemo(
    () =>
      derived
        ? {
            light: contrastReport(derived.light),
            dark: contrastReport(derived.dark),
          }
        : null,
    [derived],
  );
  const ok = !reports || [...reports.light, ...reports.dark].every((r) => r.ok);

  // The dashboard wears the theme being edited, in the mode being previewed.
  useEffect(() => {
    if (!derived) return;
    let tag = document.getElementById(STYLE_ID);
    if (!tag) {
      tag = document.createElement("style");
      tag.id = STYLE_ID;
      document.head.append(tag);
    }
    const nonce = getNonce();
    if (nonce) tag.setAttribute("nonce", nonce);
    tag.textContent = tokenBlock(":root,.dark", derived[mode]);
  }, [derived, mode]);
  useEffect(() => () => document.getElementById(STYLE_ID)?.remove(), []);

  // Saving is off while any text would be hard to read.
  useEffect(() => {
    const save = root.current?.querySelector<HTMLButtonElement>(
      "button[type=submit]",
    );
    if (save) save.disabled = !ok;
  }, [ok, spec]);

  return (
    <div ref={root} className="theme-editor">
      {children}
      <section className="theme-editor-tools" aria-label="Preview and contrast">
        <p>
          <button
            type="button"
            className="btn small"
            aria-pressed={mode === "light"}
            onClick={() => setMode("light")}
          >
            Preview light
          </button>{" "}
          <button
            type="button"
            className="btn small"
            aria-pressed={mode === "dark"}
            onClick={() => setMode("dark")}
          >
            Preview dark
          </button>
        </p>
        {reports && (
          <table className="grid" aria-label="Contrast of text colours">
            <thead>
              <tr>
                <th>Text</th>
                <th>Light</th>
                <th>Dark</th>
              </tr>
            </thead>
            <tbody>
              {reports.light.map((row, i) => {
                const other = reports.dark[i];
                const cell = (r: typeof row) => (
                  <td>
                    {r.ok ? "✓" : "✗"} {r.ratio.toFixed(1)}:1
                  </td>
                );
                return (
                  <tr key={row.pair.join("|")}>
                    <td>
                      {PAIR_LABELS[row.pair.join("|")] ?? row.pair.join(" on ")}
                    </td>
                    {cell(row)}
                    {cell(other)}
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        {!ok && (
          <p role="alert" className="error">
            Some text would be hard to read (it needs a ratio of at least
            4.5:1). Adjust the colours before saving.
          </p>
        )}
      </section>
    </div>
  );
}
