import { useMemo, type CSSProperties } from "react";
import "./theme-preview.css";
import { derive, type Tokens, type ThemeSpec } from "./theme-derive";

export { parseSpec } from "./theme-spec";

const bg = (c: string): CSSProperties => ({ backgroundColor: c });

function Swatch({ label, t }: { label: string; t: Tokens }) {
  return (
    <div
      role="img"
      aria-label={label}
      className="theme-swatch"
      style={{ backgroundColor: t["--background"], borderColor: t["--border"] }}
    >
      <div data-part="sidebar" className="ts-side" style={bg(t["--sidebar"])}>
        {[0, 1, 2].map((i) => (
          <i key={i} style={bg(t["--sidebar-foreground"])} />
        ))}
      </div>
      <div className="ts-main">
        <b style={bg(t["--foreground"])} />
        <em style={bg(t["--muted-foreground"])} />
        <div className="ts-row">
          <span
            data-part="button"
            style={{
              backgroundColor: t["--primary"],
              color: t["--primary-foreground"],
            }}
          >
            Pay
          </span>
          <span
            data-part="chip"
            style={{
              backgroundColor: t["--ui-accent"],
              color: t["--accent-foreground"],
            }}
          >
            New
          </span>
        </div>
      </div>
    </div>
  );
}

/** A theme's look in light and dark, drawn from the same derivation the dashboard uses. */
export default function ThemePreview({
  spec,
  name,
}: {
  spec: ThemeSpec;
  name: string;
}) {
  const { light, dark } = useMemo(
    () => derive(spec.seeds, spec.dark_seeds, spec.overrides),
    [spec],
  );
  return (
    <div className="theme-swatches">
      <Swatch label={`${name}, light`} t={light} />
      <Swatch label={`${name}, dark`} t={dark} />
    </div>
  );
}
