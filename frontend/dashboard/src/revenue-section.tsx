import { Suspense, lazy, useMemo, useState, type ReactNode } from "react";
import { useRouter } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { Skeleton } from "@/components/ui/skeleton";
import { textOf, type ViewNode } from "./adapter";
import { parseRevenue, type Series } from "./revenue";
import {
  NativeSelect,
  NativeSelectOption,
} from "@/components/ui/native-select";

// The chart is a separate chunk; the skeleton has its size so nothing shifts.
const RevenueChart = lazy(() => import("./revenue-chart"));

const find = (n: ViewNode, test: (n: ViewNode) => boolean): ViewNode | null => {
  if (test(n)) return n;
  for (const c of n.children) {
    const hit = find(c, test);
    if (hit) return hit;
  }
  return null;
};
const findAll = (n: ViewNode, test: (n: ViewNode) => boolean): ViewNode[] =>
  (test(n) ? [n] : []).concat(n.children.flatMap((c) => findAll(c, test)));
const hasClass = (n: ViewNode, name: string) =>
  (n.attrs.class ?? "").split(/\s+/).includes(name);

type Field = {
  name: string;
  label: string;
  value: string;
  options: { value: string; label: string }[];
};
function readSelects(form: ViewNode | null): Field[] {
  if (!form) return [];
  return findAll(form, (n) => n.tag === "select").map((s) => {
    const options = findAll(s, (n) => n.tag === "option").map((o) => ({
      value: o.attrs.value ?? textOf(o),
      label: textOf(o).trim(),
      selected: "selected" in o.attrs,
    }));
    return {
      name: s.attrs.name,
      label: s.attrs["aria-label"] ?? "",
      value: options.find((o) => o.selected)?.value ?? options[0]?.value ?? "",
      options,
    };
  });
}
function readInput(form: ViewNode | null, name: string) {
  const input =
    form && find(form, (n) => n.tag === "input" && n.attrs.name === name);
  return input?.attrs.value ?? "";
}
// Labels in the server form wrap the control ("App <select>"); take the text.
function labelFor(form: ViewNode, name: string) {
  const label = find(
    form,
    (n) =>
      n.tag === "label" &&
      !!find(n, (c) => c.attrs.name === name && c.tag !== "label"),
  );
  return label
    ? label.children
        .filter((c) => c.tag === "#text")
        .map((c) => c.text ?? "")
        .join("")
        .trim()
    : name;
}

function money(series: Series) {
  const f = new Intl.NumberFormat(series.currency === "INR" ? "en-IN" : "en", {
    style: "currency",
    currency: series.currency || "USD",
    minimumFractionDigits: series.exp,
    maximumFractionDigits: series.exp,
  });
  return (minor: number) => f.format(minor / 10 ** series.exp);
}

type Tone = "good" | "bad" | "flat";
export function chipTone(metric: string, text: string): Tone {
  const dir = /^\+/.test(text) ? 1 : /^[-−–]/.test(text) ? -1 : 0;
  if (dir === 0) return "flat";
  // More refunds is bad news; more of anything else is good.
  return (dir === 1) === (metric !== "refunds") ? "good" : "bad";
}

function Kpis({ summary, series }: { summary: ViewNode; series: Series }) {
  const fmt = money(series);
  const sums = useMemo(() => {
    const gross = series.points.reduce((a, p) => a + p.gross, 0);
    const refunds = series.points.reduce((a, p) => a + p.refunds, 0);
    return { gross, refunds, net: gross - refunds } as Record<string, number>;
  }, [series]);
  const tiles = findAll(
    summary,
    (n) => n.tag === "div" && !!find(n, (c) => c.tag === "dt"),
  );
  return (
    <div className="revenue-kpis">
      {tiles.map((tile) => {
        const label = textOf(find(tile, (n) => n.tag === "dt")!).trim();
        const metric = label.toLowerCase();
        const change = find(tile, (n) => hasClass(n, "change"));
        const text = change ? textOf(change).trim() : "";
        return (
          <div className="revenue-kpi" data-metric={metric} key={metric}>
            <span className="kpi-label">{label}</span>
            <span className="kpi-value">{fmt(sums[metric] ?? 0)}</span>
            {text && (
              <span className="kpi-chip" data-tone={chipTone(metric, text)}>
                {text}
              </span>
            )}
          </div>
        );
      })}
    </div>
  );
}

function Toolbar({
  presets,
  form,
  render,
}: {
  presets: ViewNode | null;
  form: ViewNode | null;
  render: (n: ViewNode, i?: number) => ReactNode;
}) {
  const router = useRouter();
  const selects = readSelects(form);
  const [open, setOpen] = useState(false);
  const [from, setFrom] = useState(readInput(form, "from"));
  const [to, setTo] = useState(readInput(form, "to"));
  const custom = !!presets && !find(presets, (n) => hasClass(n, "active"));

  // Always start from the page's own query so a filter never resets the range.
  function go(change: (q: URLSearchParams) => void) {
    const here = router.state.location;
    const url = new URL(here.pathname + here.searchStr, location.origin);
    change(url.searchParams);
    void router.navigate({
      to: url.pathname,
      search: Object.fromEntries(url.searchParams),
    });
  }
  const set = (q: URLSearchParams, name: string, value: string) =>
    value ? q.set(name, value) : q.delete(name);

  return (
    <div className="revenue-toolbar">
      {presets && render(presets)}
      <div className="revenue-filters-row">
        {selects.map((s) => (
          <label className="revenue-select" key={s.name}>
            <span>{form ? labelFor(form, s.name) : s.name}</span>
            <NativeSelect
              size="sm"
              aria-label={form ? labelFor(form, s.name) : s.name}
              value={s.value}
              onChange={(e) =>
                go((q) =>
                  set(
                    q,
                    s.name,
                    e.target.value === "auto" ? "" : e.target.value,
                  ),
                )
              }
            >
              {s.options.map((o) => (
                <NativeSelectOption key={o.value} value={o.value}>
                  {o.label}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </label>
        ))}
        <Popover open={open} onOpenChange={setOpen}>
          <PopoverTrigger asChild>
            <Button
              variant="outline"
              size="sm"
              className="revenue-custom"
              data-active={custom || undefined}
            >
              Custom range
            </Button>
          </PopoverTrigger>
          <PopoverContent className="revenue-popover">
            <label>
              From
              <input
                type="date"
                value={from}
                onChange={(e) => setFrom(e.target.value)}
              />
            </label>
            <label>
              To
              <input
                type="date"
                value={to}
                onChange={(e) => setTo(e.target.value)}
              />
            </label>
            <Button
              size="sm"
              onClick={() => {
                go((q) => {
                  q.set("range", "custom");
                  set(q, "from", from);
                  set(q, "to", to);
                });
                setOpen(false);
              }}
            >
              Apply
            </Button>
          </PopoverContent>
        </Popover>
      </div>
    </div>
  );
}

/**
 * The Overview revenue section, rebuilt from the server's markup:
 * toolbar, KPI tiles, chart, notes, then the table (behind a toggle) and CSV.
 * Without data rows the server layout is kept as is.
 */
export function RevenueSection({
  node,
  render,
}: {
  node: ViewNode;
  render: (n: ViewNode, i?: number) => ReactNode;
}) {
  const [table, setTable] = useState(false);
  const data = find(node, (n) => n.attrs.id === "revenue-data");
  const series = useMemo(() => (data ? parseRevenue(data) : null), [data]);
  if (!series) return <>{node.children.map(render)}</>;

  const kids = node.children;
  const heading = kids.find((n) => n.tag === "h2");
  const presets =
    kids.find((n) => n.tag === "nav" && hasClass(n, "range-presets")) ?? null;
  const form =
    kids.find((n) => n.tag === "form" && hasClass(n, "revenue-filters")) ??
    null;
  const summary = kids.find(
    (n) => n.tag === "dl" && hasClass(n, "revenue-summary"),
  );
  const wrapper = kids.find(
    (n) => !!find(n, (c) => c.attrs.id === "revenue-data"),
  );
  const csv = kids.find(
    (n) => n.tag === "p" && !!find(n, (c) => "download" in c.attrs),
  );
  const used = new Set([heading, presets, form, summary, wrapper, csv]);
  const notes = kids.filter((n) => !used.has(n) && n.tag !== "#text");

  return (
    <>
      {heading && render(heading)}
      <Toolbar
        key={`${readInput(form, "from")}|${readInput(form, "to")}`}
        presets={presets}
        form={form}
        render={render}
      />
      {summary && <Kpis summary={summary} series={series} />}
      <Suspense
        fallback={
          <div
            className="revenue-chart revenue-chart-loading"
            aria-hidden="true"
          >
            <Skeleton className="h-full w-full" />
          </div>
        }
      >
        <RevenueChart series={series} />
      </Suspense>
      {notes.length > 0 && (
        <div className="revenue-notes">{notes.map(render)}</div>
      )}
      <Button
        variant="ghost"
        size="sm"
        className="revenue-table-toggle"
        aria-expanded={table}
        onClick={() => setTable(!table)}
      >
        {table ? "Hide table" : "View as table"}
      </Button>
      <div hidden={!table}>{wrapper && render(wrapper)}</div>
      {csv && render(csv)}
    </>
  );
}
