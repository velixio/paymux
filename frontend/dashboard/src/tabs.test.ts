import { describe, expect, test } from "vitest";
import {
  MAX_UNPINNED,
  activate,
  close,
  closeOthers,
  closeRight,
  duplicate,
  initialState,
  land,
  load,
  move,
  navigate,
  open,
  reopen,
  rename,
  save,
  setPinned,
  storageKey,
  tabKey,
  type TabState,
} from "./tabs";

const none: ReadonlySet<string> = new Set();
const D = "/dashboard/";

function withTabs(n: number, now = 1): TabState {
  let s = initialState(now);
  for (let i = 1; i <= n; i++)
    s = open(s, `${D}p${i}`, `P${i}`, "new", now + i, none);
  return s;
}
const hrefs = (s: TabState) => s.tabs.map((t) => t.href);
const active = (s: TabState) => s.tabs.find((t) => t.id === s.activeId)!;

describe("tabKey", () => {
  test("tabKey ignores classic, hash and param order", () => {
    expect(tabKey("/dashboard/payments?b=2&a=1&classic=1#x")).toBe(
      tabKey("/dashboard/payments?a=1&b=2"),
    );
    expect(tabKey("/dashboard/p?a=2&a=1&b=1")).toBe("/dashboard/p?a=1&a=2&b=1");
    expect(tabKey("/dashboard/payments?a=&classic=1")).toBe(
      "/dashboard/payments",
    );
    expect(tabKey("/dashboard/payments?a=1")).not.toBe(
      tabKey("/dashboard/payments?a=2"),
    );
  });
});

describe("open", () => {
  test("initialState is one pinned Overview tab", () => {
    const s = initialState(5);
    expect(s.tabs).toHaveLength(1);
    expect(s.tabs[0]).toMatchObject({
      href: "/dashboard/overview",
      pinned: true,
      lastActive: 5,
    });
    expect(s.activeId).toBe(s.tabs[0].id);
  });

  test("open focuses an existing tab with the same key", () => {
    const s = withTabs(2);
    const first = s.tabs[1];
    const r = open(s, `${D}p1?classic=1#h`, "x", "new", 99, none);
    expect(r.tabs).toHaveLength(3);
    expect(r.activeId).toBe(first.id);
    expect(active(r).lastActive).toBe(99);
  });

  test("replace swaps the active unpinned tab", () => {
    const s = withTabs(2);
    const id = s.activeId;
    const r = open(s, `${D}other`, "Other", "replace", 50, none);
    expect(r.tabs).toHaveLength(3);
    expect(r.activeId).toBe(id);
    expect(active(r)).toMatchObject({
      href: `${D}other`,
      title: "Other",
      lastActive: 50,
    });
  });

  test("replace on a pinned active tab opens a new tab", () => {
    const s = initialState(1);
    const r = open(s, `${D}other`, "Other", "replace", 2, none);
    expect(r.tabs).toHaveLength(2);
    expect(r.tabs[0].href).toBe("/dashboard/overview");
    expect(active(r).href).toBe(`${D}other`);
  });

  test("replace on a pinned tab that only changes the query updates it in place", () => {
    const s = initialState(1);
    const r = open(s, `${D}overview?range=12m`, "Overview", "replace", 2, none);
    expect(r.tabs).toHaveLength(1);
    expect(r.tabs[0]).toMatchObject({
      href: `${D}overview?range=12m`,
      pinned: true,
      lastActive: 2,
    });
    expect(r.activeId).toBe(s.activeId);
    // A different pathname still leaves the pinned tab alone.
    expect(open(r, `${D}payments`, "P", "replace", 3, none).tabs).toHaveLength(
      2,
    );
  });

  test("new appends after the active tab", () => {
    let s = withTabs(3);
    s = activate(s, s.tabs[1].id, 60);
    const r = open(s, `${D}new`, "New", "new", 70, none);
    expect(hrefs(r)).toEqual([
      "/dashboard/overview",
      `${D}p1`,
      `${D}new`,
      `${D}p2`,
      `${D}p3`,
    ]);
    expect(active(r).href).toBe(`${D}new`);
  });

  test("11th unpinned tab evicts the least recently active clean tab", () => {
    let s = withTabs(MAX_UNPINNED);
    s = activate(s, s.tabs[1].id, 500); // p1 is now recent; p2 is oldest
    const r = open(s, `${D}extra`, "Extra", "new", 600, none);
    expect(r.tabs.filter((t) => !t.pinned)).toHaveLength(MAX_UNPINNED);
    expect(hrefs(r)).not.toContain(`${D}p2`);
    expect(hrefs(r)).toContain(`${D}p1`);
    expect(r.closed[0].href).toBe(`${D}p2`);
  });

  test("eviction skips dirty and pinned tabs; all dirty -> tab count exceeds cap", () => {
    const s = withTabs(MAX_UNPINNED);
    const dirty = new Set(s.tabs.map((t) => t.id));
    const r = open(s, `${D}extra`, "Extra", "new", 600, dirty);
    expect(r.tabs).toHaveLength(MAX_UNPINNED + 2);
    expect(r.tabs[0].pinned).toBe(true);

    const some = new Set(s.tabs.slice(1, 3).map((t) => t.id)); // p1, p2 dirty
    const r2 = open(s, `${D}extra`, "Extra", "new", 600, some);
    expect(hrefs(r2)).not.toContain(`${D}p3`);
    expect(hrefs(r2)).toContain(`${D}p1`);
  });
});

describe("close", () => {
  test("close of the active tab activates its right neighbour, else left", () => {
    let s = withTabs(3);
    s = activate(s, s.tabs[2].id, 70); // p2, has right neighbour p3
    expect(active(close(s, s.activeId)).href).toBe(`${D}p3`);
    s = activate(s, s.tabs[3].id, 80); // p3, last
    expect(active(close(s, s.activeId)).href).toBe(`${D}p2`);
  });

  test("closing a non-active tab keeps the active one", () => {
    const s = withTabs(2);
    const r = close(s, s.tabs[1].id);
    expect(r.activeId).toBe(s.activeId);
    expect(r.closed).toHaveLength(1);
  });

  test("closing an unknown id is a no-op", () => {
    const s = withTabs(1);
    expect(close(s, "nope")).toBe(s);
  });

  test("closing the last tab yields initialState", () => {
    const s = initialState(7);
    const r = close(s, s.activeId);
    expect(r.tabs).toHaveLength(1);
    expect(r.tabs[0]).toMatchObject({
      href: "/dashboard/overview",
      pinned: true,
    });
  });

  test("closed list is capped at 10, most recent first", () => {
    let s = withTabs(12);
    for (let i = 0; i < 12; i++) s = close(s, s.tabs[s.tabs.length - 1].id);
    expect(s.closed).toHaveLength(10);
    expect(s.closed[0].href).toBe("/dashboard/overview");
  });

  test("closeOthers keeps pinned and the chosen tab", () => {
    const s = withTabs(3);
    const keep = s.tabs[2].id;
    const r = closeOthers(s, keep);
    expect(hrefs(r)).toEqual(["/dashboard/overview", `${D}p2`]);
    expect(r.activeId).toBe(keep);
    expect(r.closed).toHaveLength(2);
  });

  test("closeRight closes tabs right of the chosen one", () => {
    const s = withTabs(3);
    const r = closeRight(s, s.tabs[1].id);
    expect(hrefs(r)).toEqual(["/dashboard/overview", `${D}p1`]);
    expect(r.activeId).toBe(s.tabs[1].id);
    expect(closeRight(s, "nope")).toBe(s);
    expect(closeOthers(s, "nope")).toBe(s);
  });
});

describe("reopen, duplicate, pin, move, rename, navigate", () => {
  test("reopen restores the most recent closed tab", () => {
    let s = withTabs(2);
    s = close(s, s.tabs[2].id);
    const r = reopen(s, 900);
    expect(active(r).href).toBe(`${D}p2`);
    expect(r.closed).toHaveLength(0);
    expect(reopen(r, 901)).toBe(r);
  });

  test("reopen of a key that is already open just focuses it", () => {
    let s = withTabs(1);
    const gone = s.tabs[1];
    s = close(s, gone.id);
    s = open(s, gone.href, "again", "new", 20, none);
    const r = reopen(s, 30);
    expect(r.tabs).toHaveLength(s.tabs.length);
    expect(r.closed).toHaveLength(0);
  });

  test("duplicate creates a second tab with the same href to the right", () => {
    const s = withTabs(2);
    const src = s.tabs[1];
    const r = duplicate(s, src.id, 300);
    expect(r.tabs).toHaveLength(4);
    expect(r.tabs[2].href).toBe(src.href);
    expect(r.tabs[2].id).not.toBe(src.id);
    expect(r.activeId).toBe(r.tabs[2].id);
    expect(duplicate(s, "nope", 1)).toBe(s);
  });

  test("tab ids stay unique", () => {
    const s = duplicate(withTabs(2), "x", 1);
    const r = duplicate(duplicate(s, s.activeId, 5), s.activeId, 5);
    expect(new Set(r.tabs.map((t) => t.id)).size).toBe(r.tabs.length);
  });

  test("pinned tabs stay left of unpinned after move", () => {
    let s = withTabs(3);
    s = setPinned(s, s.tabs[3].id, true);
    expect(hrefs(s).slice(0, 2)).toEqual(["/dashboard/overview", `${D}p3`]);
    const moved = move(s, s.tabs[3].id, 0);
    expect(moved.tabs[0].pinned).toBe(true);
    expect(moved.tabs.map((t) => t.pinned)).toEqual([true, true, false, false]);
    const m2 = move(s, s.tabs[0].id, 3);
    expect(m2.tabs.map((t) => t.pinned)).toEqual([true, true, false, false]);
    expect(hrefs(move(s, s.tabs[2].id, 3))).toEqual(
      hrefs(s).slice(0, 2).concat(`${D}p2`, `${D}p1`),
    );
    expect(move(s, "nope", 0)).toBe(s);
  });

  test("setPinned false moves the tab into the unpinned region", () => {
    let s = setPinned(withTabs(1), withTabs(1).tabs[1].id, true);
    s = setPinned(s, s.tabs[0].id, false);
    expect(s.tabs.map((t) => t.pinned)).toEqual([true, false]);
    expect(s.tabs[0].href).toBe(`${D}p1`);
  });

  test("rename and navigate update one tab", () => {
    const s = withTabs(1);
    const id = s.tabs[1].id;
    expect(rename(s, id, "Renamed").tabs[1].title).toBe("Renamed");
    const n = navigate(s, id, `${D}z`, "Z");
    expect(n.tabs[1]).toMatchObject({ href: `${D}z`, title: "Z", id });
    expect(navigate(s, "nope", "/dashboard/z", "Z")).toBe(s);
    expect(rename(s, "nope", "x")).toBe(s);
  });

  test("activate ignores unknown ids", () => {
    const s = withTabs(1);
    expect(activate(s, "nope", 5)).toBe(s);
  });
});

describe("persistence", () => {
  const key = storageKey("u1", "live");
  const store = (v: string | null) => ({ getItem: () => v });

  test("storageKey", () => {
    expect(key).toBe("paymux-tabs:u1:live");
  });

  test("load ignores malformed JSON", () => {
    expect(load(store("{nope"), key, 5)).toEqual(initialState(5));
    expect(load(store('{"tabs":"x"}'), key, 5)).toEqual(initialState(5));
    expect(load(store(null), key, 5)).toEqual(initialState(5));
    expect(load(null, key, 5)).toEqual(initialState(5));
  });

  test("load survives throwing storage", () => {
    const boom = {
      getItem: () => {
        throw new Error("denied");
      },
    };
    expect(load(boom, key, 5)).toEqual(initialState(5));
  });

  test("load drops tabs whose href is not under /dashboard", () => {
    const raw = JSON.stringify({
      tabs: [
        { id: "a", href: "/dashboard/payments", title: "Pay", pinned: true },
        {
          id: "b",
          href: "https://evil.example.com/dashboard/x",
          title: "E",
          pinned: false,
        },
        { id: "c", href: "/dashboardx", title: "E", pinned: false },
        { id: "d", href: "/other", title: "E", pinned: false },
        { id: 5, href: "/dashboard/x", title: "bad", pinned: false },
        { id: "e", href: "/dashboard/x", title: "X", pinned: false },
      ],
    });
    const s = load(store(raw), key, 9);
    expect(s.tabs.map((t) => t.id)).toEqual(["a", "e"]);
    expect(s.activeId).toBe("a");
    expect(s.tabs[0].lastActive).toBe(9);
    expect(
      load(
        store(
          '{"tabs":[{"id":"d","href":"/other","title":"","pinned":false}]}',
        ),
        key,
        1,
      ),
    ).toEqual(initialState(1));
  });

  test("save writes only id, href, title, pinned", () => {
    let written = "";
    const s = withTabs(1);
    save({ setItem: (_k, v) => (written = v) }, key, s);
    const parsed = JSON.parse(written);
    expect(Object.keys(parsed)).toEqual(["tabs"]);
    for (const t of parsed.tabs)
      expect(Object.keys(t).sort()).toEqual(["href", "id", "pinned", "title"]);
    expect(parsed.tabs).toHaveLength(2);
    expect(
      load({ getItem: () => written }, key, 3).tabs.map((t) => t.href),
    ).toEqual(hrefs(s));
  });

  test("save tolerates throwing or missing storage", () => {
    const s = withTabs(0);
    expect(() => save(null, key, s)).not.toThrow();
    expect(() =>
      save(
        {
          setItem: () => {
            throw new Error("quota");
          },
        },
        key,
        s,
      ),
    ).not.toThrow();
  });
});

describe("land (boot on a URL)", () => {
  test("focuses the tab that already shows the URL", () => {
    const s = withTabs(1);
    const r = land(s, `${D}p1`, "P1", 50);
    expect(r.tabs).toHaveLength(2);
    expect(r.activeId).toBe(s.tabs[1].id);
  });

  test("a URL on the same pathname as a tab navigates that tab in place", () => {
    const s = initialState(1);
    const r = land(s, `${D}overview?range=12m`, "Overview", 50);
    expect(r.tabs).toHaveLength(1);
    expect(r.tabs[0]).toMatchObject({
      href: `${D}overview?range=12m`,
      pinned: true,
      lastActive: 50,
    });
    expect(r.activeId).toBe(s.activeId);
  });

  test("prefers a pinned tab, then the first, when several share the pathname", () => {
    let s = withTabs(1);
    s = duplicate(s, s.tabs[1].id, 5); // two /p1 tabs
    s = setPinned(s, s.tabs[2].id, true);
    const r = land(s, `${D}p1?x=1`, "P1", 60);
    expect(r.tabs.find((t) => t.href === `${D}p1?x=1`)!.pinned).toBe(true);
    expect(r.tabs).toHaveLength(s.tabs.length);
  });

  test("an unknown pathname adds a tab beside the active one", () => {
    const s = withTabs(1);
    const r = land(s, `${D}elsewhere`, "Elsewhere", 70);
    expect(r.tabs).toHaveLength(3);
    expect(r.tabs.at(-1)!.href).toBe(`${D}elsewhere`);
    expect(r.activeId).toBe(r.tabs.at(-1)!.id);
  });
});
