import { useEffect, type DependencyList, type RefObject } from "react";
import { overflowEdges } from "./toolbar";

/**
 * Marks a horizontal scroller with data-fade-start / data-fade-end while it
 * hides content on that side (CSS turns them into soft fades), and scrolls
 * the element matching `current` into view.
 */
export function useEdgeFade(
  ref: RefObject<HTMLElement | null>,
  current: string,
  deps: DependencyList,
) {
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const update = () => {
      const edges = overflowEdges(
        el.scrollLeft,
        el.scrollWidth,
        el.clientWidth,
      );
      el.toggleAttribute("data-fade-start", edges.start);
      el.toggleAttribute("data-fade-end", edges.end);
    };
    el.querySelector<HTMLElement>(current)?.scrollIntoView?.({
      block: "nearest",
      inline: "nearest",
    });
    update();
    el.addEventListener("scroll", update, { passive: true });
    const resize =
      typeof ResizeObserver === "function" ? new ResizeObserver(update) : null;
    resize?.observe(el);
    return () => {
      el.removeEventListener("scroll", update);
      resize?.disconnect();
    };
    // The caller decides when to re-measure.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
}
