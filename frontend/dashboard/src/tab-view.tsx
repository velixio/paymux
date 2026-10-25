import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type MutableRefObject,
} from "react";
import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { fetchDashboard } from "./adapter";
import { PageContent } from "./render";
import { queryKey, RuntimeProvider } from "./runtime";
import { useTableNav } from "./use-table-nav";
import type { Tab } from "./tabs";

/** A hidden, clean tab is unmounted after this long without a visit. */
export const EVICT_AFTER_MS = 10 * 60 * 1000;
/** Returning to a tab refetches it when its data is older than this. */
export const REFRESH_AFTER_MS = 30_000;

type BodyProps = {
  tab: Tab;
  active: boolean;
  dirty: boolean;
  onDirty: (dirty: boolean) => void;
  onOnce: (once: boolean) => void;
  scroll: MutableRefObject<number>;
};

function TabBody({ tab, active, dirty, onDirty, onOnce, scroll }: BodyProps) {
  const href = tab.href;
  const { data, error, refetch, dataUpdatedAt } = useQuery({
    queryKey: queryKey(href),
    queryFn: ({ signal }) => fetchDashboard(href, signal),
    // The view decides when data is stale, so a dirty tab is never refetched.
    staleTime: Infinity,
    enabled: active,
  });
  const section = useRef<HTMLElement>(null);
  useTableNav(section, active);

  // What is rendered. A hidden tab ignores data updates (another tab on the
  // same page may refetch) so its form state survives; it picks up the newest
  // data when shown, unless it holds unsaved edits.
  const view = useRef({ data, updatedAt: dataUpdatedAt });
  const wasActive = useRef(active);
  if (active && !(dirty && !wasActive.current))
    view.current = { data, updatedAt: dataUpdatedAt };
  wasActive.current = active;
  const shown = view.current;

  useEffect(() => {
    if (
      active &&
      data &&
      !dirty &&
      Date.now() - dataUpdatedAt > REFRESH_AFTER_MS
    )
      void refetch();
    // Only a change of visibility triggers the check.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active]);

  const once = !!shown.data?.once;
  useEffect(() => {
    onOnce(once);
  }, [once, onOnce]);

  const loaded = !!shown.data;
  useLayoutEffect(() => {
    if (active && loaded && section.current)
      section.current.scrollTop = scroll.current;
  }, [active, loaded, scroll]);
  // In-tab navigation starts at the top.
  const shownHref = useRef(href);
  useLayoutEffect(() => {
    if (shownHref.current === href) return;
    shownHref.current = href;
    scroll.current = 0;
    if (section.current) section.current.scrollTop = 0;
  }, [href, scroll]);

  return (
    <>
      <section
        className="react-content"
        ref={section}
        id={active ? "react-workspace" : undefined}
        tabIndex={-1}
        hidden={!active}
        data-tab={tab.id}
        onScroll={() => {
          if (active && section.current)
            scroll.current = section.current.scrollTop;
        }}
      >
        {shown.data ? (
          <RuntimeProvider
            key={shown.updatedAt}
            page={shown.data}
            guard={active}
            pinned={tab.pinned}
            onDirtyChange={onDirty}
          >
            <PageContent
              key={
                shown.data.href +
                "-" +
                shown.data.content.map((n) => n.attrs.id || n.tag).join("")
              }
              nodes={shown.data.content}
            />
          </RuntimeProvider>
        ) : error ? (
          <div className="workspace-load-error">
            <Alert variant="destructive">
              <AlertTitle>Unable to load workspace</AlertTitle>
              <AlertDescription>{error.message}</AlertDescription>
            </Alert>
            <Button onClick={() => refetch()}>Try again</Button>
            <a href={href + (href.includes("?") ? "&" : "?") + "classic=1"}>
              Use the classic dashboard
            </a>
          </div>
        ) : (
          <div
            className="workspace-loading"
            role={active ? "status" : undefined}
            aria-label={active ? "Loading workspace" : undefined}
          >
            <Skeleton className="h-12 w-56" />
            <Skeleton className="h-80 w-full" />
          </div>
        )}
      </section>
    </>
  );
}

/**
 * One workspace tab. Hidden tabs stay mounted so edits and scroll survive,
 * except pages that show a one-time secret (unmounted at once) and clean tabs
 * left unvisited for EVICT_AFTER_MS (one timeout each, never an interval).
 */
export function TabView({
  tab,
  active,
  onDirty,
}: {
  tab: Tab;
  active: boolean;
  onDirty: (id: string, dirty: boolean) => void;
}) {
  const [dirty, setDirty] = useState(false);
  const [once, setOnce] = useState(false);
  const [evicted, setEvicted] = useState(false);
  const scroll = useRef(0);
  const id = tab.id;

  useEffect(() => {
    if (active) {
      setEvicted(false);
      return;
    }
    if (dirty) return;
    const timer = setTimeout(() => setEvicted(true), EVICT_AFTER_MS);
    return () => clearTimeout(timer);
  }, [active, dirty]);

  const reportDirty = useCallback(
    (d: boolean) => {
      setDirty(d);
      onDirty(id, d);
    },
    [id, onDirty],
  );
  if (!active && (once || evicted)) return null;
  return (
    <TabBody
      tab={tab}
      active={active}
      dirty={dirty}
      onDirty={reportDirty}
      onOnce={setOnce}
      scroll={scroll}
    />
  );
}
