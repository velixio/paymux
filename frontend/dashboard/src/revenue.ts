import { extractRows, type ViewNode } from "./adapter";

export type Point = {
  start: string;
  gross: number;
  refunds: number;
  net: number;
  prevNet: number;
};
export type Series = {
  currency: string;
  exp: number;
  bucket: string;
  converted: boolean;
  estimated: boolean;
  points: Point[];
};

const num = (value: string | undefined) => {
  const n = Number(value);
  return Number.isFinite(n) ? n : 0;
};

// Reads the server-rendered `table#revenue-data`: amounts are minor units in
// data attributes; the visible cells are only for people.
export function parseRevenue(node: ViewNode): Series | null {
  const rows = extractRows(node).filter((r) => r.attrs["data-start"]);
  if (rows.length === 0) return null;
  const a = node.attrs;
  const exp = Number(a["data-exp"]);
  return {
    currency: a["data-currency"] ?? "",
    exp: Number.isInteger(exp) && exp >= 0 ? exp : 2,
    bucket: a["data-bucket"] ?? "day",
    converted: a["data-converted"] === "true",
    estimated: a["data-estimated"] === "true",
    points: rows.map((r) => {
      const gross = num(r.attrs["data-gross"]);
      const refunds = num(r.attrs["data-refunds"]);
      return {
        start: r.attrs["data-start"],
        gross,
        refunds,
        net: gross - refunds,
        prevNet:
          num(r.attrs["data-prev-gross"]) - num(r.attrs["data-prev-refunds"]),
      };
    }),
  };
}

// A vertical axis that always contains zero, with a "nice" step (1, 2 or 5
// times a power of ten, never below one minor unit). `y` maps a value to a
// pixel from the top: max at 0, min at `height`.
export function scale(values: number[], height: number) {
  const lo = Math.min(0, ...values);
  const hi = Math.max(0, ...values);
  const span = hi === lo ? 1 : hi - lo;
  const raw = span / 4;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const r = raw / mag;
  const step = Math.max(1, (r <= 1 ? 1 : r <= 2 ? 2 : r <= 5 ? 5 : 10) * mag);
  const first = Math.floor(lo / step);
  const last = Math.max(Math.ceil(hi / step), first + 1);
  const ticks: number[] = [];
  for (let k = first; k <= last; k++) ticks.push(k * step);
  const min = ticks[0];
  const max = ticks[ticks.length - 1];
  return { min, max, ticks, y: (v: number) => ((max - v) / (max - min)) * height };
}

const trim = (n: number) => n.toFixed(1).replace(/\.0$/, "");

// Short axis labels. INR uses Indian units (K, lakh, crore); other currencies
// use the locale's compact notation.
export function compactMoney(minor: number, currency: string, exp: number): string {
  const major = minor / 10 ** exp;
  if (currency === "INR") {
    const abs = Math.abs(major);
    const sign = major < 0 ? "-" : "";
    const body =
      abs >= 1e7
        ? `${trim(abs / 1e7)} Cr`
        : abs >= 1e5
          ? `${trim(abs / 1e5)} L`
          : abs >= 1e3
            ? `${trim(abs / 1e3)}K`
            : String(Math.round(abs * 100) / 100);
    return `${sign}₹${body}`;
  }
  return new Intl.NumberFormat("en", {
    style: "currency",
    currency,
    notation: "compact",
    minimumFractionDigits: 0,
    maximumFractionDigits: 1,
  }).format(major);
}
