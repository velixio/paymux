import { act, renderHook } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  shouldRefreshDashboardHref,
  useDashboardLiveEvents,
} from "./live-events";

class FakeEventSource {
  static current: FakeEventSource | undefined;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: (() => void) | null = null;
  closed = false;
  constructor(readonly url: string) {
    FakeEventSource.current = this;
  }
  close() {
    this.closed = true;
  }
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  FakeEventSource.current = undefined;
});

test("only operational dashboard pages are live", () => {
  expect(shouldRefreshDashboardHref("/dashboard/overview")).toBe(true);
  expect(shouldRefreshDashboardHref("/dashboard/analytics?range=7d")).toBe(
    true,
  );
  expect(shouldRefreshDashboardHref("/dashboard/payments/pay_1")).toBe(true);
  expect(shouldRefreshDashboardHref("/dashboard/settings")).toBe(false);
  expect(shouldRefreshDashboardHref("/docs")).toBe(false);
});

test("an SSE event invalidates clean operational tabs as one burst", async () => {
  const client = new QueryClient();
  for (const href of [
    "/dashboard/overview",
    "/dashboard/payments",
    "/dashboard/settings",
  ])
    client.setQueryData(["dashboard", href], { href });

  const { result, unmount } = renderHook(() =>
    useDashboardLiveEvents(client, new Set(["/dashboard/payments"])),
  );
  expect(FakeEventSource.current?.url).toBe("/dashboard/events");
  act(() => FakeEventSource.current?.onopen?.());
  expect(result.current).toBe("live");
  act(() => {
    FakeEventSource.current?.onmessage?.();
    FakeEventSource.current?.onmessage?.();
    vi.advanceTimersByTime(100);
  });
  await Promise.resolve();

  expect(
    client.getQueryState(["dashboard", "/dashboard/overview"])?.isInvalidated,
  ).toBe(true);
  expect(
    client.getQueryState(["dashboard", "/dashboard/payments"])?.isInvalidated,
  ).toBe(false);
  expect(
    client.getQueryState(["dashboard", "/dashboard/settings"])?.isInvalidated,
  ).toBe(false);
  unmount();
  expect(FakeEventSource.current?.closed).toBe(true);
});
