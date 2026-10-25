// Pure workspace tab model: every function returns a new state and never
// touches React, the DOM or the clock.
export type Tab = {
  id: string;
  href: string;
  title: string;
  pinned: boolean;
  lastActive: number;
};
export type TabState = { tabs: Tab[]; activeId: string; closed: Tab[] };

export const MAX_UNPINNED = 10;
const MAX_CLOSED = 10;
const HOME = "/dashboard/overview";

// Identity of a page: pathname plus identifying query, sorted. The `classic`
// switch, the side-pane `peek`, empty values and the hash do not identify a
// page.
export function tabKey(href: string): string {
  const u = new URL(href, "http://tabs.invalid");
  const q = [...u.searchParams.entries()]
    .filter(([k, v]) => k !== "classic" && k !== "peek" && v !== "")
    .sort(([a, x], [b, y]) =>
      a === b ? x.localeCompare(y) : a.localeCompare(b),
    )
    .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`)
    .join("&");
  return q ? `${u.pathname}?${q}` : u.pathname;
}

// Deterministic unique id: `t${now}-${n}` with the smallest unused n.
function newId(s: TabState, now: number): string {
  const used = new Set([...s.tabs, ...s.closed].map((t) => t.id));
  let n = 0;
  while (used.has(`t${now}-${n}`)) n++;
  return `t${now}-${n}`;
}

export function initialState(now: number): TabState {
  const id = `t${now}-0`;
  return {
    tabs: [
      { id, href: HOME, title: "Overview", pinned: true, lastActive: now },
    ],
    activeId: id,
    closed: [],
  };
}

// Pinned tabs always sit left of unpinned ones (stable within each group).
const settle = (tabs: Tab[]): Tab[] => [
  ...tabs.filter((t) => t.pinned),
  ...tabs.filter((t) => !t.pinned),
];

const pushClosed = (closed: Tab[], gone: Tab[]): Tab[] =>
  [...gone.reverse(), ...closed].slice(0, MAX_CLOSED);

const has = (s: TabState, id: string) => s.tabs.some((t) => t.id === id);

function evict(s: TabState, dirty: ReadonlySet<string>): TabState {
  const unpinned = s.tabs.filter((t) => !t.pinned);
  if (unpinned.length <= MAX_UNPINNED) return s;
  const victim = unpinned
    .filter((t) => t.id !== s.activeId && !dirty.has(t.id))
    .sort((a, b) => a.lastActive - b.lastActive)[0];
  if (!victim) return s; // everything is dirty: allow exceeding the cap
  return evict(
    {
      ...s,
      tabs: s.tabs.filter((t) => t !== victim),
      closed: pushClosed(s.closed, [victim]),
    },
    dirty,
  );
}

function insertAfter(s: TabState, afterId: string, tab: Tab): Tab[] {
  const i = s.tabs.findIndex((t) => t.id === afterId);
  return settle([...s.tabs.slice(0, i + 1), tab, ...s.tabs.slice(i + 1)]);
}

const samePath = (a: string, b: string) =>
  new URL(a, "http://tabs.invalid").pathname ===
  new URL(b, "http://tabs.invalid").pathname;

export function open(
  s: TabState,
  href: string,
  title: string,
  how: "replace" | "new",
  now: number,
  dirtyIds: ReadonlySet<string>,
): TabState {
  const key = tabKey(href);
  const same = s.tabs.find((t) => tabKey(t.href) === key);
  if (same) return activate(s, same.id, now);
  const cur = s.tabs.find((t) => t.id === s.activeId);
  // A pinned tab only keeps users from navigating away from its page; a new
  // query on the same page (a range preset, a filter) stays in the tab.
  if (how === "replace" && cur && (!cur.pinned || samePath(cur.href, href))) {
    return {
      ...s,
      tabs: s.tabs.map((t) =>
        t === cur ? { ...t, href, title, lastActive: now } : t,
      ),
    };
  }
  const tab: Tab = {
    id: newId(s, now),
    href,
    title,
    pinned: false,
    lastActive: now,
  };
  return evict(
    { ...s, tabs: insertAfter(s, s.activeId, tab), activeId: tab.id },
    dirtyIds,
  );
}

const pathOf = (href: string) => new URL(href, "http://tabs.invalid").pathname;

// Boot on a URL (full page load, restored tabs): focus the tab that shows it;
// else navigate a tab on the same page in place (pinned first), so a reload of
// /dashboard/overview?range=12m updates the Overview tab instead of adding one;
// else open a new tab beside the active one.
export function land(
  s: TabState,
  href: string,
  title: string,
  now: number,
): TabState {
  const same = s.tabs.find((t) => tabKey(t.href) === tabKey(href));
  if (same) return activate(s, same.id, now);
  const path = pathOf(href);
  const onPage = s.tabs.filter((t) => pathOf(t.href) === path);
  const match = onPage.find((t) => t.pinned) ?? onPage[0];
  if (match)
    return {
      ...s,
      activeId: match.id,
      tabs: s.tabs.map((t) =>
        t === match ? { ...t, href, title, lastActive: now } : t,
      ),
    };
  return open(s, href, title, "new", now, new Set());
}

export function activate(s: TabState, id: string, now: number): TabState {
  if (!has(s, id)) return s;
  return {
    ...s,
    activeId: id,
    tabs: s.tabs.map((t) => (t.id === id ? { ...t, lastActive: now } : t)),
  };
}

// Closing the last remaining tab yields a fresh Overview tab; the closed tab is
// still remembered so it can be reopened.
function remove(s: TabState, gone: Tab[], nextActive: string): TabState {
  const goneIds = new Set(gone.map((t) => t.id));
  const tabs = s.tabs.filter((t) => !goneIds.has(t.id));
  const closed = pushClosed(s.closed, [...gone]);
  if (tabs.length === 0) {
    const fresh = initialState(Math.max(...gone.map((t) => t.lastActive)) + 1);
    return { ...fresh, closed };
  }
  return { tabs, activeId: nextActive, closed };
}

export function close(s: TabState, id: string): TabState {
  const i = s.tabs.findIndex((t) => t.id === id);
  if (i < 0) return s;
  const neighbour = s.tabs[i + 1] ?? s.tabs[i - 1];
  return remove(s, [s.tabs[i]], id === s.activeId ? neighbour?.id : s.activeId);
}

export function closeOthers(s: TabState, id: string): TabState {
  if (!has(s, id)) return s;
  return remove(
    s,
    s.tabs.filter((t) => t.id !== id && !t.pinned),
    id,
  );
}

export function closeRight(s: TabState, id: string): TabState {
  const i = s.tabs.findIndex((t) => t.id === id);
  if (i < 0) return s;
  // Pinned tabs stay, as in a browser.
  return remove(
    s,
    s.tabs.slice(i + 1).filter((t) => !t.pinned),
    id,
  );
}

export function reopen(
  s: TabState,
  now: number,
  dirtyIds: ReadonlySet<string> = new Set(),
): TabState {
  const [tab, ...closed] = s.closed;
  if (!tab) return s;
  const same = s.tabs.find((t) => tabKey(t.href) === tabKey(tab.href));
  if (same) return activate({ ...s, closed }, same.id, now);
  const back: Tab = { ...tab, lastActive: now };
  // The reopened tab counts towards the cap like any newly opened one.
  return evict(
    { tabs: settle([...s.tabs, back]), activeId: back.id, closed },
    dirtyIds,
  );
}

export function duplicate(s: TabState, id: string, now: number): TabState {
  const src = s.tabs.find((t) => t.id === id);
  if (!src) return s;
  const copy: Tab = {
    ...src,
    id: newId(s, now),
    pinned: false,
    lastActive: now,
  };
  return { ...s, tabs: insertAfter(s, id, copy), activeId: copy.id };
}

export function setPinned(s: TabState, id: string, pinned: boolean): TabState {
  return {
    ...s,
    tabs: settle(s.tabs.map((t) => (t.id === id ? { ...t, pinned } : t))),
  };
}

export function move(s: TabState, id: string, toIndex: number): TabState {
  const tab = s.tabs.find((t) => t.id === id);
  if (!tab) return s;
  const rest = s.tabs.filter((t) => t !== tab);
  rest.splice(Math.max(0, Math.min(toIndex, rest.length)), 0, tab);
  return { ...s, tabs: settle(rest) };
}

const patch = (s: TabState, id: string, p: Partial<Tab>): TabState =>
  has(s, id)
    ? { ...s, tabs: s.tabs.map((t) => (t.id === id ? { ...t, ...p } : t)) }
    : s;

export const rename = (s: TabState, id: string, title: string): TabState =>
  patch(s, id, { title });

// In-tab navigation: the tab keeps its id and position.
export const navigate = (
  s: TabState,
  id: string,
  href: string,
  title: string,
): TabState => patch(s, id, { href, title });

export const storageKey = (user: string, mode: string): string =>
  `paymux-tabs:${user}:${mode}`;

const underDashboard = (href: string) =>
  href === "/dashboard" || /^\/dashboard[/?#]/.test(href);

// Only tabs persist; the active tab is not stored, so the first tab becomes
// active and the caller opens the current location on top.
export function load(
  storage: Pick<Storage, "getItem"> | null,
  key: string,
  now: number,
): TabState {
  try {
    const raw = storage?.getItem(key);
    const list: unknown = raw ? JSON.parse(raw).tabs : null;
    if (!Array.isArray(list)) return initialState(now);
    const tabs: Tab[] = list
      .filter(
        (t) =>
          t &&
          typeof t.id === "string" &&
          typeof t.href === "string" &&
          typeof t.title === "string" &&
          typeof t.pinned === "boolean" &&
          underDashboard(t.href),
      )
      .map((t) => ({
        id: t.id,
        href: t.href,
        title: t.title,
        pinned: t.pinned,
        lastActive: now,
      }));
    if (tabs.length === 0) return initialState(now);
    const settled = settle(tabs);
    return { tabs: settled, activeId: settled[0].id, closed: [] };
  } catch {
    return initialState(now);
  }
}

export function save(
  storage: Pick<Storage, "setItem"> | null,
  key: string,
  s: TabState,
): void {
  try {
    const tabs = s.tabs.map(({ id, href, title, pinned }) => ({
      id,
      href,
      title,
      pinned,
    }));
    storage?.setItem(key, JSON.stringify({ tabs }));
  } catch {
    // Storage may be full or blocked; tabs then last for this page view only.
  }
}
