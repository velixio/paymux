import { useEffect, useRef, useState } from "react";
import type { QueryClient } from "@tanstack/react-query";

export type LiveConnection = "connecting" | "live" | "unavailable";

const LIVE_SECTIONS = new Set([
  "overview",
  "analytics",
  "payments",
  "subscriptions",
  "invoices",
  "disputes",
  "reconciliation",
  "reports",
  "alerts",
]);

export function shouldRefreshDashboardHref(href: string): boolean {
  const path = new URL(href, "http://localhost").pathname;
  if (!path.startsWith("/dashboard/")) return false;
  return LIVE_SECTIONS.has(path.split("/")[2] || "");
}

// Opens one authenticated SSE stream for the workspace and coalesces bursts
// into one refresh of every clean operational tab.
export function useDashboardLiveEvents(
  client: QueryClient,
  dirtyHrefs: ReadonlySet<string>,
): LiveConnection {
  const [connection, setConnection] = useState<LiveConnection>("connecting");
  const dirtyHrefsRef = useRef(dirtyHrefs);
  dirtyHrefsRef.current = dirtyHrefs;

  useEffect(() => {
    if (typeof EventSource === "undefined") {
      setConnection("unavailable");
      return;
    }
    const source = new EventSource("/dashboard/events");
    let refresh: ReturnType<typeof setTimeout> | undefined;
    source.onopen = () => setConnection("live");
    source.onerror = () => setConnection("connecting");
    source.onmessage = () => {
      if (refresh !== undefined) return;
      refresh = setTimeout(() => {
        refresh = undefined;
        void client.invalidateQueries({
          predicate: (query) => {
            const href = query.queryKey[1];
            return (
              typeof href === "string" &&
              !dirtyHrefsRef.current.has(href) &&
              shouldRefreshDashboardHref(href)
            );
          },
        });
      }, 100);
    };
    return () => {
      clearTimeout(refresh);
      source.close();
    };
  }, [client]);
  return connection;
}
