import { useEffect, useRef, type RefObject } from "react";
import { isTyping } from "./keys";
import { nextRow } from "./table-nav";
import { useOptionalTabs } from "./tabs-context";

const KEYS = {
  ArrowDown: "down",
  ArrowUp: "up",
  Home: "first",
  End: "last",
} as const;

// Keyboard navigation of the first data table in the visible tab: j/k always,
// the arrow keys, Home and End once a row has focus (so the page still
// scrolls), Enter opens the row's first link, Alt+Enter in a new tab.
export function useTableNav(
  root: RefObject<HTMLElement | null>,
  active: boolean,
) {
  const tabs = useOptionalTabs();
  const tabsRef = useRef(tabs);
  tabsRef.current = tabs;

  useEffect(() => {
    if (!active) return;
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as Element | null;
      if (e.ctrlKey || e.metaKey || isTyping(target)) return;
      if (target?.closest?.('[role="dialog"], [role="menu"]')) return;
      const table = root.current?.querySelector("table[data-nav]");
      if (!table) return;
      const rows = [...table.querySelectorAll<HTMLElement>("tbody tr")].filter(
        (tr) => tr.querySelector("a[href]"),
      );
      const at = rows.findIndex((tr) => tr.contains(document.activeElement));

      let move: "down" | "up" | "first" | "last" | null = null;
      if (!e.altKey && !e.shiftKey) {
        if (e.code === "KeyJ") move = "down";
        else if (e.code === "KeyK") move = "up";
        else if (at >= 0 && e.key in KEYS)
          move = KEYS[e.key as keyof typeof KEYS];
      }
      if (move) {
        e.preventDefault();
        const to = nextRow(at, rows.length, move);
        if (to < 0) return;
        for (const tr of rows) tr.removeAttribute("data-nav-row");
        const row = rows[to];
        row.setAttribute("data-nav-row", "");
        row.tabIndex = -1;
        row.focus();
        row.scrollIntoView?.({ block: "nearest" });
        return;
      }
      if (e.key === "Enter" && at >= 0) {
        const link = rows[at].querySelector<HTMLAnchorElement>("a[href]")!;
        // A focused link or button already handles a plain Enter itself.
        if (!e.altKey && target?.closest("a, button")) return;
        const url = new URL(link.getAttribute("href")!, location.origin);
        e.preventDefault();
        tabsRef.current?.openHref(
          url.pathname + url.search,
          e.altKey ? "new" : "replace",
        );
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [active, root]);
}
