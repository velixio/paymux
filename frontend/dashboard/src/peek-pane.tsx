import {
  useCallback,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from "react";
import { useQuery } from "@tanstack/react-query";
import { useLocation, useRouter } from "@tanstack/react-router";
import { ExternalLink, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import { fetchDashboard } from "./adapter";
import { isTyping } from "./keys";
import {
  clampWidth,
  loadWidth,
  peekOf,
  saveWidth,
  withPeek,
  isRecordPath,
  setPeekGuard,
} from "./peek";
import { PageContent, RenderNode } from "./render";
import { queryKey, RuntimeProvider } from "./runtime";
import { splitHead } from "./sections";
import { extractToolbar } from "./toolbar";
import { useOptionalTabs } from "./tabs-context";
import "./peek-pane.css";

const WIDE = "(min-width: 1100px)";
const wideQuery = () =>
  typeof matchMedia === "function" ? matchMedia(WIDE) : null;
function useWide() {
  return useSyncExternalStore(
    (notify) => {
      const query = wideQuery();
      query?.addEventListener("change", notify);
      return () => query?.removeEventListener("change", notify);
    },
    () => !!wideQuery()?.matches,
    () => false,
  );
}

const store = () => {
  try {
    return localStorage;
  } catch {
    return null;
  }
};

/**
 * Wraps a workspace tab's page. With ?peek=<record> in the URL (and room for
 * it) the record shows in a pane at the right, beside the list.
 */
export function PeekFrame({
  active,
  children,
}: {
  active: boolean;
  children: ReactNode;
}) {
  const loc = useLocation();
  const router = useRouter();
  const tabs = useOptionalTabs();
  const wide = useWide();
  const record = active && wide ? peekOf(loc.searchStr) : null;
  const frame = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(() => loadWidth(store()));
  const dirty = useRef(false);
  const [confirm, setConfirm] = useState<(() => void) | null>(null);
  const onDirty = useCallback((d: boolean) => {
    dirty.current = d;
  }, []);

  const go = useCallback(
    (next: string | null, replace: boolean) => {
      const here = router.state.location;
      const to = new URL(
        withPeek(here.pathname + here.searchStr, next),
        location.origin,
      );
      void router.navigate({
        to: to.pathname,
        search: Object.fromEntries(to.searchParams),
        replace,
        ignoreBlocker: true,
      });
    },
    [router],
  );
  // Leaving the record with unsaved edits asks first.
  const keep = useRef<(() => void) | null>(null);
  const guarded = useCallback((run: () => void, onKeep?: () => void) => {
    if (!dirty.current) return run();
    keep.current = onKeep ?? null;
    setConfirm(() => run);
  }, []);
  // Anything that switches the pane to another record asks first.
  useEffect(() => {
    if (!record) return;
    setPeekGuard((run) => guarded(run));
    return () => setPeekGuard(null);
  }, [record, guarded]);
  const close = useCallback(() => guarded(() => go(null, true)), [guarded, go]);
  const openInTab = useCallback(() => {
    if (record) guarded(() => tabs?.openHref(record, "new"));
  }, [record, guarded, tabs]);

  // The clicked row stays highlighted.
  useEffect(() => {
    for (const tr of frame.current?.querySelectorAll("tbody tr") ?? []) {
      const on =
        !!record &&
        [...tr.querySelectorAll("a[href]")].some(
          (a) => a.getAttribute("href") === record,
        );
      tr.toggleAttribute("data-peek-row", on);
    }
  });

  // Escape closes the pane, unless a menu or dialog has the keyboard.
  useEffect(() => {
    if (!record) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || e.defaultPrevented) return;
      if (
        (e.target as Element | null)?.closest?.(
          '[role="dialog"], [role="menu"]',
        )
      )
        return;
      e.preventDefault();
      close();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [record, close]);

  const recordLink = (row: Element | null) =>
    [...(row?.querySelectorAll("a[href]") ?? [])]
      .map((a) => a.getAttribute("href")!)
      .find((h) => isRecordPath(h));
  // j/k (and the arrow keys) move the selection: the pane follows the row.
  const onFocus = (e: React.FocusEvent) => {
    if (!record) return;
    const row = (e.target as Element).closest?.("tbody tr[data-nav-row]");
    const href = recordLink(row ?? null);
    if (!href || href === record) return;
    // Cancelling leaves the selection on the record the pane still shows.
    guarded(
      () => go(href, true),
      () => {
        const rows = [
          ...(frame.current?.querySelectorAll<HTMLElement>("tbody tr") ?? []),
        ];
        const current = rows.find((tr) => recordLink(tr) === record);
        for (const tr of rows) tr.removeAttribute("data-nav-row");
        current?.setAttribute("data-nav-row", "");
        current?.focus();
      },
    );
  };
  // Enter on the selected row opens it in a tab.
  const onKeyDown = (e: ReactKeyboardEvent) => {
    if (!record || e.key !== "Enter" || e.altKey || e.ctrlKey || e.metaKey)
      return;
    const target = e.target as Element;
    if (target.closest("a, button") || isTyping(target)) return;
    const row = target.closest("tbody tr[data-nav-row]");
    const href = recordLink(row);
    if (!href) return;
    e.preventDefault();
    e.stopPropagation();
    guarded(() => tabs?.openHref(href, "new"));
  };

  return (
    <div
      ref={frame}
      className={cn("peek-frame", record && "peek-open")}
      onFocus={onFocus}
      onKeyDown={onKeyDown}
    >
      {children}
      {record && (
        <PeekPane
          key={record}
          record={record}
          width={width}
          onWidth={(w) => {
            setWidth(w);
            saveWidth(store(), w);
          }}
          onClose={close}
          onOpenInTab={openInTab}
          onDirty={onDirty}
        />
      )}
      <Dialog open={!!confirm} onOpenChange={(o) => !o && setConfirm(null)}>
        <DialogContent
          onCloseAutoFocus={(e) => {
            // Back on the row that was selected before the cancelled move.
            const restore = keep.current;
            keep.current = null;
            if (restore) {
              e.preventDefault();
              restore();
            }
          }}
        >
          <DialogHeader>
            <DialogTitle>Discard unsaved changes?</DialogTitle>
            <DialogDescription>
              The record in the side pane has changes that have not been saved.
              Keep editing, or discard them and continue.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirm(null)}>
              Keep editing
            </Button>
            <Button
              onClick={() => {
                const run = confirm;
                keep.current = null;
                dirty.current = false;
                setConfirm(null);
                run?.();
              }}
            >
              Discard and continue
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function PeekPane({
  record,
  width,
  onWidth,
  onClose,
  onOpenInTab,
  onDirty,
}: {
  record: string;
  width: number;
  onWidth: (w: number) => void;
  onClose: () => void;
  onOpenInTab: () => void;
  onDirty: (dirty: boolean) => void;
}) {
  const { data, error, refetch } = useQuery({
    queryKey: queryKey(record),
    queryFn: ({ signal }) => fetchDashboard(record, signal),
    staleTime: 0,
  });
  // The record's own toolbar becomes the pane header: its title (with the
  // status pill) and action links; the back link has no place in a pane.
  const parts = data ? extractToolbar(data.content) : null;
  const body = parts ? parts.rest : data ? splitHead(data.content).rest : [];

  const drag = useRef<{ x: number; w: number } | null>(null);
  const resize = (e: ReactPointerEvent<HTMLDivElement>) => {
    if (!drag.current) return;
    onWidth(clampWidth(drag.current.w + (drag.current.x - e.clientX)));
  };
  const step = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const by = { ArrowLeft: 20, ArrowRight: -20 }[e.key];
    if (!by) return;
    e.preventDefault();
    onWidth(clampWidth(width + by));
  };

  const buttons = (
    <>
      <Button
        variant="outline"
        size="sm"
        className="peek-open-tab"
        onClick={onOpenInTab}
      >
        <ExternalLink size={12} aria-hidden="true" /> Open in tab
      </Button>
      <Button
        variant="ghost"
        size="icon"
        className="peek-close"
        aria-label="Close preview"
        onClick={onClose}
      >
        <X size={14} aria-hidden="true" />
      </Button>
    </>
  );
  return (
    <aside
      className="peek-pane"
      aria-label="Record preview"
      style={{ width }}
      data-state="open"
    >
      <div
        className="peek-resize"
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize preview"
        aria-valuemin={360}
        aria-valuemax={640}
        aria-valuenow={width}
        tabIndex={0}
        onPointerDown={(e) => {
          drag.current = { x: e.clientX, w: width };
          e.currentTarget.setPointerCapture?.(e.pointerId);
        }}
        onPointerMove={resize}
        onPointerUp={() => (drag.current = null)}
        onKeyDown={step}
      />
      {data ? (
        <RuntimeProvider
          page={data}
          guard={false}
          inPane
          onDirtyChange={onDirty}
        >
          <div className="peek-head">
            <h2 className="peek-title">
              {parts
                ? parts.toolbar.title.map((n, i) => (
                    <RenderNode key={i} node={n} />
                  ))
                : record.split("/").at(-1)}
            </h2>
            {parts && parts.toolbar.actions.length > 0 && (
              <div className="peek-actions">
                {parts.toolbar.actions.map((n, i) => (
                  <RenderNode key={i} node={n} />
                ))}
              </div>
            )}
            {buttons}
          </div>
          <div className="react-content peek-body">
            <PageContent nodes={body} href={record} />
          </div>
        </RuntimeProvider>
      ) : (
        <>
          <div className="peek-head">
            <h2 className="peek-title">{record.split("/").at(-1)}</h2>
            {buttons}
          </div>
          <div className="react-content peek-body">
            {error ? (
              <div className="peek-error">
                <p>Unable to load this record.</p>
                <Button variant="outline" size="sm" onClick={() => refetch()}>
                  Try again
                </Button>
              </div>
            ) : (
              <div role="status" aria-label="Loading record">
                <Skeleton className="h-6 w-40" />
                <Skeleton className="mt-3 h-40 w-full" />
              </div>
            )}
          </div>
        </>
      )}
    </aside>
  );
}
