import { useEffect, useMemo, useRef, useState } from "react";
import { compactMoney, scale, type Series } from "./revenue";

const DEFAULT_W = 720;
const MIN_W = 320;
const H = 260;
const M = { top: 10, right: 8, bottom: 26, left: 56 };
const PLOT_H = H - M.top - M.bottom;
const MONTHS = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
  "Dec",
];

// The server's RFC 3339 start is already in the reporting zone, so labels read
// its own date fields instead of converting through the browser's zone.
function parts(start: string) {
  return {
    y: start.slice(0, 4),
    m: Number(start.slice(5, 7)) - 1,
    d: Number(start.slice(8, 10)),
    hh: start.slice(11, 13),
  };
}
function axisLabel(start: string, bucket: string) {
  const { y, m, d, hh } = parts(start);
  if (bucket === "hour") return `${hh}:00`;
  if (bucket === "month")
    return m === 0 ? `${MONTHS[m]} ${y.slice(2)}` : MONTHS[m];
  if (bucket === "quarter") return `Q${Math.floor(m / 3) + 1} ${y.slice(2)}`;
  return `${d} ${MONTHS[m]}`;
}
function fullLabel(start: string, bucket: string) {
  const { y, m, d, hh } = parts(start);
  if (bucket === "hour") return `${d} ${MONTHS[m]} ${y}, ${hh}:00`;
  if (bucket === "month") return `${MONTHS[m]} ${y}`;
  if (bucket === "quarter") return `Q${Math.floor(m / 3) + 1} ${y}`;
  if (bucket === "week") return `Week of ${d} ${MONTHS[m]} ${y}`;
  return `${d} ${MONTHS[m]} ${y}`;
}

export default function RevenueChart({ series }: { series: Series }) {
  const { points, currency, exp, bucket } = series;
  const [hover, setHover] = useState<number | null>(null);
  // Draw at the container's real width so text stays crisp and bars fill it.
  const plot = useRef<HTMLDivElement>(null);
  const [W, setW] = useState(DEFAULT_W);
  useEffect(() => {
    const el = plot.current;
    if (!el) return;
    const fit = () => {
      const width = el.getBoundingClientRect().width;
      if (width > 0) setW(Math.max(MIN_W, Math.round(width)));
    };
    fit();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(fit);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  const PLOT_W = W - M.left - M.right;
  const bars = useRef<(SVGGElement | null)[]>([]);
  const money = useMemo(() => {
    const f = new Intl.NumberFormat(currency === "INR" ? "en-IN" : "en", {
      style: "currency",
      currency: currency || "USD",
      minimumFractionDigits: exp,
      maximumFractionDigits: exp,
    });
    return (minor: number) => f.format(minor / 10 ** exp);
  }, [currency, exp]);
  const axis = useMemo(
    () =>
      scale(
        points.flatMap((p) => [p.gross, p.net, p.prevNet, 0]),
        PLOT_H,
      ),
    [points],
  );
  const band = PLOT_W / points.length;
  const cx = (i: number) => M.left + band * (i + 0.5);
  const y = (v: number) => M.top + axis.y(v);
  const every = Math.ceil(points.length / 8);
  const shown = hover === null ? null : points[hover];

  function move(event: React.KeyboardEvent, i: number) {
    const to = { ArrowRight: i + 1, ArrowLeft: i - 1 }[event.key];
    if (to === undefined || to < 0 || to >= points.length) return;
    event.preventDefault();
    bars.current[to]?.focus();
  }

  return (
    <figure className="revenue-chart" aria-label="Revenue chart">
      <div className="revenue-chart-plot" ref={plot}>
        <svg viewBox={`0 0 ${W} ${H}`} width={W} height={H}>
          {axis.ticks.map((t) => (
            <g key={t}>
              <line
                className="rev-grid"
                x1={M.left}
                x2={W - M.right}
                y1={y(t)}
                y2={y(t)}
              />
              <text
                className="rev-tick"
                x={M.left - 6}
                y={y(t)}
                textAnchor="end"
                dominantBaseline="middle"
              >
                {compactMoney(t, currency, exp)}
              </text>
            </g>
          ))}
          <line
            data-zero-line
            className="rev-zero"
            x1={M.left}
            x2={W - M.right}
            y1={y(0)}
            y2={y(0)}
          />
          {points.map((p, i) => {
            const grossW = band * 0.7;
            const netW = band * 0.46;
            const rect = (v: number, w: number, x = cx(i) - w / 2) => ({
              x,
              y: Math.min(y(v), y(0)),
              width: w,
              height: Math.abs(y(v) - y(0)),
            });
            const refundTop = Math.min(y(p.gross), y(p.net));
            return (
              <g
                key={p.start}
                data-bar
                ref={(el) => {
                  bars.current[i] = el;
                }}
                role="img"
                tabIndex={0}
                aria-label={`${fullLabel(p.start, bucket)}: net ${money(p.net)}, gross ${money(p.gross)}, refunds ${money(p.refunds)}`}
                onMouseEnter={() => setHover(i)}
                onMouseLeave={() => setHover(null)}
                onFocus={() => setHover(i)}
                onBlur={() => setHover(null)}
                onKeyDown={(e) => move(e, i)}
              >
                <rect
                  className="rev-hit"
                  x={M.left + band * i}
                  y={M.top}
                  width={band}
                  height={PLOT_H}
                />
                <rect className="rev-gross" {...rect(p.gross, grossW)} />
                <rect className="rev-net" {...rect(p.net, netW)} />
                {p.refunds > 0 && (
                  <rect
                    className="rev-refunds"
                    x={cx(i) + netW / 2}
                    y={refundTop}
                    width={Math.max(2, band * 0.1)}
                    height={Math.abs(y(p.gross) - y(p.net))}
                  />
                )}
              </g>
            );
          })}
          <polyline
            className="rev-prev"
            fill="none"
            points={points.map((p, i) => `${cx(i)},${y(p.prevNet)}`).join(" ")}
          />
          {points.map(
            (p, i) =>
              i % every === 0 && (
                <text
                  key={p.start}
                  data-x-label
                  className="rev-tick"
                  x={cx(i)}
                  y={H - 8}
                  textAnchor="middle"
                >
                  {axisLabel(p.start, bucket)}
                </text>
              ),
          )}
        </svg>
        {shown && hover !== null && (
          <div
            role="tooltip"
            className="revenue-tooltip"
            style={{ left: `${(cx(hover) / W) * 100}%` }}
          >
            <strong>{fullLabel(shown.start, bucket)}</strong>
            <dl>
              <dt>Gross</dt>
              <dd>{money(shown.gross)}</dd>
              <dt>Refunds</dt>
              <dd>{money(shown.refunds)}</dd>
              <dt>Net</dt>
              <dd>{money(shown.net)}</dd>
              <dt>Previous period net</dt>
              <dd>{money(shown.prevNet)}</dd>
            </dl>
          </div>
        )}
      </div>
      <figcaption className="revenue-legend">
        <span className="rev-key rev-key-net">Net</span>
        <span className="rev-key rev-key-gross">Gross</span>
        <span className="rev-key rev-key-refunds">Refunds</span>
        <span className="rev-key rev-key-prev">Previous period</span>
        {series.estimated && <span>Includes estimated conversions</span>}
      </figcaption>
    </figure>
  );
}
