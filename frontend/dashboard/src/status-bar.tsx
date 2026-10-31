import { useEffect, useState } from "react";
import { TriangleAlert } from "lucide-react";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { agoText } from "./status";
import type { LiveConnection } from "./live-events";

const TICK_MS = 10_000;

// Re-renders every 10 s with one timeout at a time, none while the page is
// hidden, and immediately when it becomes visible again.
function useNow() {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const arm = () => {
      if (document.hidden) return;
      timer = setTimeout(() => {
        setNow(Date.now());
        arm();
      }, TICK_MS);
    };
    const onVisibility = () => {
      clearTimeout(timer);
      if (document.hidden) return;
      setNow(Date.now());
      arm();
    };
    arm();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, []);
  return now;
}

function useOnline() {
  const [online, setOnline] = useState(() => navigator.onLine !== false);
  useEffect(() => {
    const on = () => setOnline(true);
    const off = () => setOnline(false);
    window.addEventListener("online", on);
    window.addEventListener("offline", off);
    return () => {
      window.removeEventListener("online", on);
      window.removeEventListener("offline", off);
    };
  }, []);
  return online;
}

// The 24 px bar under the workspace: connection, last refresh of the active
// tab, dev warnings (one chip, a popover lists them) and shortcut hints.
export function StatusBar({
  updatedAt,
  warnings,
  onShortcuts,
  liveConnection,
}: {
  updatedAt: number | undefined;
  warnings: string[];
  onShortcuts: () => void;
  liveConnection?: LiveConnection;
}) {
  const online = useOnline();
  const now = useNow();
  const live = online && liveConnection === "live";
  const connectionLabel = !online
    ? "Offline"
    : live
      ? "Live"
      : liveConnection === "connecting"
        ? "Reconnecting"
        : liveConnection === "unavailable"
          ? "Unavailable"
          : "Connected";
  return (
    <footer className="status-bar">
      <span
        className={
          live || (online && liveConnection === undefined)
            ? "status-online"
            : online && liveConnection === "connecting"
              ? "status-connecting"
              : "status-offline"
        }
        aria-live="polite"
        aria-atomic="true"
      >
        <span className="status-dot" aria-hidden="true" />
        {connectionLabel}
      </span>
      {updatedAt !== undefined && (
        <span>Updated {agoText(now - updatedAt)}</span>
      )}
      {warnings.length > 0 && (
        <Popover>
          <PopoverTrigger asChild>
            <button type="button" className="status-warning">
              <TriangleAlert size={12} aria-hidden="true" />
              {warnings.length === 1
                ? warnings[0]
                : `${warnings.length} warnings`}
            </button>
          </PopoverTrigger>
          <PopoverContent side="top" align="start" className="status-popover">
            <ul aria-label="Warnings">
              {warnings.map((message, i) => (
                <li className="workspace-warning" key={i}>
                  <TriangleAlert size={13} aria-hidden="true" />
                  {message}
                </li>
              ))}
            </ul>
          </PopoverContent>
        </Popover>
      )}
      <span className="status-hints">
        j/k move · Enter open · Alt+Enter new tab
      </span>
      <button
        type="button"
        className="shortcuts-link"
        aria-label="Keyboard shortcuts"
        onClick={onShortcuts}
      >
        ? shortcuts
      </button>
    </footer>
  );
}
