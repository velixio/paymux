// Presentation-only polish of the server's page tree, applied before React
// renders it: readable money and dates, status words as pills, and a proper
// empty state. The server markup (and the no-JS view) stays as it is.
import { textOf, type ViewNode } from "./adapter";

const MONEY = /^(-?)(\d+)(?:\.(\d+))? ([A-Z]{3})$/;

/** "2750.00 INR" → "₹2,750.00"; null when the text is not an amount. */
export function formatMoneyText(s: string): string | null {
  const m = MONEY.exec(s.trim());
  if (!m) return null;
  const [, sign, whole, frac = "", currency] = m;
  try {
    const out = new Intl.NumberFormat(currency === "INR" ? "en-IN" : "en-US", {
      style: "currency",
      currency,
      minimumFractionDigits: frac.length,
      maximumFractionDigits: frac.length,
    }).format(Number(`${whole}.${frac || "0"}`));
    return sign + out;
  } catch {
    return null; // not a currency Intl knows
  }
}

const MONTHS = "Jan Feb Mar Apr May Jun Jul Aug Sep Oct Nov Dec".split(" ");
const DATE = /^(\d{4})-(\d{2})-(\d{2})(?: (\d{2}):(\d{2})(?::\d{2})?)?$/;

/**
 * "2026-10-25 08:46:28" → "25 Oct 2026, 08:46". The server already renders
 * the configured time zone, so the parts are only reworded, never converted.
 */
export function formatDateText(s: string): string | null {
  const m = DATE.exec(s.trim());
  if (!m) return null;
  const [, y, mo, d, h, mi] = m;
  const month = MONTHS[Number(mo) - 1];
  if (!month || Number(d) < 1 || Number(d) > 31) return null;
  const date = `${Number(d)} ${month} ${y}`;
  return h ? `${date}, ${h}:${mi}` : date;
}

const STATUS_HEAD = /^(status|state|2fa|level|severity|result|last result)$/i;
const STATUS_WORD = /^[a-z][a-z_ ]{0,23}$/i;

const text = (t: string): ViewNode => ({
  tag: "#text",
  attrs: {},
  children: [],
  text: t,
});
const span = (cls: string, label: string, title?: string): ViewNode => ({
  tag: "span",
  attrs: title ? { class: cls, title } : { class: cls },
  children: [text(label)],
});

// A value cell (td or dd) holding only text: format money and dates.
function polishValue(n: ViewNode): ViewNode {
  if (n.children.length !== 1 || n.children[0].tag !== "#text") return n;
  const raw = n.children[0].text ?? "";
  const money = formatMoneyText(raw);
  if (money) return { ...n, children: [span("money", money, raw.trim())] };
  const when = formatDateText(raw);
  if (when) return { ...n, children: [span("when", when, raw.trim())] };
  return n;
}

function statusCell(n: ViewNode): ViewNode {
  if (n.children.length !== 1 || n.children[0].tag !== "#text") return n;
  const word = (n.children[0].text ?? "").trim();
  if (!STATUS_WORD.test(word)) return n;
  const key = word.toLowerCase().replace(/\s+/g, "_");
  return { ...n, children: [span(`status status-${key}`, word)] };
}

function polishTable(t: ViewNode): ViewNode {
  const head = t.children.find((c) => c.tag === "thead");
  const heads =
    head?.children
      .find((c) => c.tag === "tr")
      ?.children.filter((c) => c.tag === "th")
      .map((th) => textOf(th).trim()) ?? [];
  const statusCols = new Set(
    heads.flatMap((h, i) => (STATUS_HEAD.test(h) ? [i] : [])),
  );
  const row = (tr: ViewNode): ViewNode => {
    const cells = tr.children.filter((c) => c.tag === "td");
    // A single spanning "No … yet." row is the table's empty state.
    if (
      cells.length === 1 &&
      heads.length > 1 &&
      (cells[0].attrs.colspan || /\bmuted\b/.test(cells[0].attrs.class ?? ""))
    )
      return { ...tr, attrs: { ...tr.attrs, class: "empty-row" } };
    let col = 0;
    return {
      ...tr,
      children: tr.children.map((c) => {
        if (c.tag !== "td") return c;
        const i = col++;
        return statusCols.has(i) ? statusCell(c) : polish(c);
      }),
    };
  };
  return {
    ...t,
    children: t.children.map((c) =>
      c.tag === "tbody"
        ? {
            ...c,
            children: c.children.map((tr) => (tr.tag === "tr" ? row(tr) : tr)),
          }
        : polish(c),
    ),
  };
}

function polish(n: ViewNode): ViewNode {
  if (n.tag === "#text") return n;
  if (n.tag === "table") return polishTable(n);
  if (n.tag === "td" || n.tag === "dd") {
    const done = polishValue(n);
    if (done !== n) return done;
  }
  if (n.tag === "form" || n.tag === "textarea" || n.tag === "select") return n;
  return { ...n, children: n.children.map(polish) };
}

export function polishNodes(nodes: ViewNode[]): ViewNode[] {
  return nodes.map(polish);
}
