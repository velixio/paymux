import {
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
  type ComponentType,
  type PointerEvent as ReactPointerEvent,
} from "react";
import { X } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";
import { cn } from "@/lib/utils";
import { useRuntime } from "./runtime";
import { useTabs } from "./tabs-context";
import { useEdgeFade } from "./use-edge-fade";
import type { Tab } from "./tabs";

type Icon = ComponentType<{ size?: number }>;

const NARROW = "(max-width: 639px)";
const narrowQuery = () =>
  typeof matchMedia === "function" ? matchMedia(NARROW) : null;
function useNarrow() {
  return useSyncExternalStore(
    (notify) => {
      const query = narrowQuery();
      query?.addEventListener("change", notify);
      return () => query?.removeEventListener("change", notify);
    },
    () => !!narrowQuery()?.matches,
    () => false,
  );
}

// Pointer movement below this many pixels is a click, not a drag.
const DRAG_SLOP = 4;

export function TabStrip({ iconFor }: { iconFor: (href: string) => Icon }) {
  const api = useTabs();
  const { copy } = useRuntime();
  const narrow = useNarrow();
  const list = useRef<HTMLDivElement>(null);
  const [menuId, setMenuId] = useState<string | null>(null);
  const [dragId, setDragId] = useState<string | null>(null);
  const [sheet, setSheet] = useState(false);
  const stop = useRef<(() => void) | null>(null);
  const justDragged = useRef(false);
  const { tabs, activeId } = api.state;

  useEffect(() => () => stop.current?.(), []);
  // Keep the active tab in view; fade the side that hides more tabs.
  useEdgeFade(list, '[aria-selected="true"]', [activeId, narrow, tabs.length]);

  if (narrow)
    return (
      <div className="tab-strip tab-strip-compact">
        <Sheet open={sheet} onOpenChange={setSheet}>
          <SheetTrigger asChild>
            <Button variant="ghost" size="sm">
              {tabs.length} open
            </Button>
          </SheetTrigger>
          <SheetContent side="bottom" className="tab-sheet">
            <SheetHeader>
              <SheetTitle>Open pages</SheetTitle>
            </SheetHeader>
            <ul className="tab-sheet-list">
              {tabs.map((tab) => (
                <li
                  key={tab.id}
                  className={tab.id === activeId ? "active" : ""}
                >
                  <button
                    type="button"
                    onClick={() => {
                      api.activate(tab.id);
                      setSheet(false);
                    }}
                  >
                    {tab.title}
                  </button>
                  {!tab.pinned && (
                    <button
                      type="button"
                      aria-label={`Close ${tab.title}`}
                      onClick={() => api.close(tab.id)}
                    >
                      <X size={14} />
                    </button>
                  )}
                </li>
              ))}
            </ul>
          </SheetContent>
        </Sheet>
      </div>
    );

  function startDrag(event: ReactPointerEvent, tab: Tab) {
    if (event.button !== 0 || (event.target as Element).closest(".tab-close"))
      return;
    const startX = event.clientX;
    let moved = false;
    const targetIndex = (x: number) => {
      const rects = [
        ...(list.current?.querySelectorAll('[role="tab"]') ?? []),
      ].map((el) => el.getBoundingClientRect());
      const hit = rects.findIndex((r) => x < r.right);
      return hit < 0 ? rects.length - 1 : hit;
    };
    const onMove = (e: PointerEvent) => {
      if (!moved && Math.abs(e.clientX - startX) > DRAG_SLOP) {
        moved = true;
        setDragId(tab.id);
      }
    };
    const onUp = (e: PointerEvent) => {
      stop.current?.();
      if (!moved) return;
      justDragged.current = true;
      setTimeout(() => (justDragged.current = false), 0);
      setDragId(null);
      api.move(tab.id, targetIndex(e.clientX));
    };
    stop.current = () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      stop.current = null;
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
  }

  function arrows(event: React.KeyboardEvent) {
    const step = { ArrowRight: 1, ArrowLeft: -1 }[event.key];
    if (!step) return;
    const els = [
      ...(list.current?.querySelectorAll<HTMLElement>('[role="tab"]') ?? []),
    ];
    const at = els.indexOf(document.activeElement as HTMLElement);
    const next = (at + step + els.length) % els.length;
    if (at < 0) return;
    event.preventDefault();
    els[next].focus();
    api.activate(tabs[next].id);
  }

  return (
    <div
      className="tab-strip"
      role="tablist"
      aria-label="Open pages"
      ref={list}
      onKeyDown={arrows}
    >
      {tabs.map((tab) => {
        const Icon = iconFor(tab.href);
        const active = tab.id === activeId;
        return (
          <div
            key={tab.id}
            className={cn(
              "tab",
              active && "active",
              tab.pinned && "pinned",
              dragId === tab.id && "dragging",
            )}
          >
            <DropdownMenu
              open={menuId === tab.id}
              onOpenChange={(open) => setMenuId(open ? tab.id : null)}
            >
              <DropdownMenuTrigger asChild>
                <span className="tab-anchor" aria-hidden="true" />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="start" className="tab-menu">
                <DropdownMenuItem onSelect={() => api.close(tab.id)}>
                  Close
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => api.closeOthers(tab.id)}>
                  Close others
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => api.closeRight(tab.id)}>
                  Close to the right
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem onSelect={() => api.pin(tab.id, !tab.pinned)}>
                  {tab.pinned ? "Unpin" : "Pin"}
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => api.duplicate(tab.id)}>
                  Duplicate
                </DropdownMenuItem>
                <DropdownMenuItem
                  onSelect={() => void copy(location.origin + tab.href)}
                >
                  Copy link
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => api.reload(tab.id)}>
                  Reload
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
            <button
              type="button"
              role="tab"
              aria-selected={active}
              aria-label={tab.pinned ? tab.title : undefined}
              title={tab.title}
              tabIndex={active ? 0 : -1}
              onPointerDown={(e) => startDrag(e, tab)}
              onMouseDown={(e) => e.button === 1 && e.preventDefault()}
              onClick={() => !justDragged.current && api.activate(tab.id)}
              onAuxClick={(e) => {
                if (e.button !== 1) return;
                e.preventDefault();
                api.close(tab.id);
              }}
              onContextMenu={(e) => {
                e.preventDefault();
                setMenuId(tab.id);
              }}
            >
              <Icon size={14} />
              {!tab.pinned && <span className="tab-title">{tab.title}</span>}
              {api.dirtyIds.has(tab.id) && (
                <span className="tab-dirty" aria-hidden="true" />
              )}
            </button>
            {!tab.pinned && (
              <button
                type="button"
                className="tab-close"
                aria-label={`Close ${tab.title}`}
                tabIndex={-1}
                onClick={() => api.close(tab.id)}
              >
                <X size={12} />
              </button>
            )}
          </div>
        );
      })}
    </div>
  );
}
