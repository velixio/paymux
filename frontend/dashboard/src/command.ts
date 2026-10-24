// Pure logic of the command bar: what a typed query can open, how items are
// ranked, and the per-user list of recently opened items.
export type Item = {
  id: string;
  label: string;
  hint?: string;
  href?: string;
  action?: "toggle-theme" | "switch-mode" | "reopen-tab";
};

const MAX_RECENT = 8;
const EMAIL = /^[^@\s/]+@[^@\s/]+\.[^@\s/]+$/;

// Record id prefixes and the page that shows them.
const RECORDS: { prefix: string; kind: string; path: string }[] = [
  { prefix: "pay_", kind: "payment", path: "payments" },
  { prefix: "sub_", kind: "subscription", path: "subscriptions" },
  { prefix: "inv_", kind: "invoice", path: "invoices" },
  { prefix: "crn_", kind: "credit note", path: "credit-notes" },
  { prefix: "dsp_", kind: "dispute", path: "disputes" },
];

const search = (path: string, kind: string, q: string): Item => ({
  id: `search:${path}:${q}`,
  label: `Search ${kind} for "${q}"`,
  hint: `/dashboard/${path}`,
  href: `/dashboard/${path}?q=${encodeURIComponent(q)}`,
});

export function intents(query: string): Item[] {
  const q = query.trim();
  if (!q) return [];
  for (const r of RECORDS) {
    if (q.startsWith(r.prefix) && /^[A-Za-z0-9_]+$/.test(q) && q.length > r.prefix.length) {
      return [
        {
          id: `record:${q}`,
          label: `Open ${r.kind} ${q}`,
          hint: `/dashboard/${r.path}/${q}`,
          href: `/dashboard/${r.path}/${q}`,
        },
      ];
    }
  }
  if (EMAIL.test(q)) return [search("payments", "payments", q), search("subscriptions", "subscriptions", q)];
  return [search("payments", "payments", q), search("invoices", "invoices", q)];
}

// Lower is better; null means no match. Label beats hint; within a label a
// prefix beats a substring beats a scattered subsequence.
function score(item: Item, q: string): number | null {
  const subsequence = (text: string): number | null => {
    let pos = -1;
    let gaps = 0;
    for (const ch of q) {
      const next = text.indexOf(ch, pos + 1);
      if (next < 0) return null;
      if (pos >= 0) gaps += next - pos - 1;
      pos = next;
    }
    return gaps;
  };
  const label = item.label.toLowerCase();
  const at = label.indexOf(q);
  if (at === 0) return 0;
  if (at > 0) return 1 + at / 1000;
  const sub = subsequence(label);
  if (sub !== null) return 2 + sub / 1000;
  const hint = (item.hint ?? "").toLowerCase();
  if (hint.includes(q)) return 3;
  const hsub = subsequence(hint);
  return hsub === null ? null : 4 + hsub / 1000;
}

export function rank(items: Item[], query: string): Item[] {
  const q = query.trim().toLowerCase();
  if (!q) return items;
  return items
    .map((item, i) => ({ item, i, s: score(item, q) }))
    .filter((x): x is { item: Item; i: number; s: number } => x.s !== null)
    .sort((a, b) => a.s - b.s || a.i - b.i)
    .map((x) => x.item);
}

const key = (user: string) => `paymux-recent:${user}`;

const valid = (x: unknown): x is Item =>
  typeof x === "object" && x !== null && typeof (x as Item).id === "string" && typeof (x as Item).label === "string";

export function recent(storage: Pick<Storage, "getItem"> | null, user: string): Item[] {
  try {
    const raw = storage?.getItem(key(user));
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter(valid).slice(0, MAX_RECENT) : [];
  } catch {
    return [];
  }
}

export function remember(storage: Pick<Storage, "getItem" | "setItem"> | null, user: string, item: Item): void {
  try {
    if (!storage) return;
    const next = [item, ...recent(storage, user).filter((i) => i.id !== item.id)].slice(0, MAX_RECENT);
    storage.setItem(key(user), JSON.stringify(next));
  } catch {
    /* storage unavailable: recent items are a convenience */
  }
}
