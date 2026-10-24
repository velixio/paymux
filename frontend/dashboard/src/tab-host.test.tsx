import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  act,
  cleanup,
  fireEvent,
  screen,
  waitFor,
} from "@testing-library/react";
import {
  api,
  calls,
  handlers,
  link,
  loginHtml,
  mount,
  pageHtml,
  resetHost,
  sectionOf,
  storageName,
  tabByHref,
} from "./host.test.helpers";

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

test("opening a second page keeps the first mounted but hidden", async () => {
  await mount();
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  expect(sectionOf("Overview body")?.hidden).toBe(true);
  expect(sectionOf("Payments body")?.hidden).toBe(false);
  expect(api.state.tabs.map((t) => t.href)).toEqual([
    "/dashboard/overview",
    "/dashboard/payments",
  ]);
  expect(document.querySelectorAll("#react-workspace")).toHaveLength(1);
});

test("hidden tab does not fetch", async () => {
  localStorage.setItem(
    storageName,
    JSON.stringify({
      tabs: [
        {
          id: "a",
          href: "/dashboard/overview",
          title: "Overview",
          pinned: true,
        },
        {
          id: "b",
          href: "/dashboard/payments",
          title: "Payments",
          pinned: false,
        },
      ],
    }),
  );
  await mount();
  expect(api.state.tabs).toHaveLength(2);
  expect(calls["/dashboard/overview"]).toBe(1);
  expect(calls["/dashboard/payments"]).toBeUndefined();
  expect(screen.queryByText("Payments body")).toBeNull();
});

test("returning after 31s refetches, before 30s does not", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-25T10:00:00Z"));
  await mount();
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  const overview = tabByHref("/dashboard/overview").id;
  const payments = tabByHref("/dashboard/payments").id;

  vi.setSystemTime(new Date("2026-10-25T10:00:29Z"));
  act(() => api.activate(overview));
  await waitFor(() => expect(sectionOf("Overview body")?.hidden).toBe(false));
  expect(calls["/dashboard/overview"]).toBe(1);

  vi.setSystemTime(new Date("2026-10-25T10:00:31Z"));
  act(() => api.activate(payments));
  await waitFor(() => expect(calls["/dashboard/payments"]).toBe(2));
  expect(calls["/dashboard/overview"]).toBe(1);
  act(() => api.activate(overview));
  await waitFor(() => expect(calls["/dashboard/overview"]).toBe(2));
});

test("once page unmounts when hidden", async () => {
  handlers["/dashboard/apps"] = (n) =>
    pageHtml(
      "Apps",
      n === 1
        ? "<div data-once><code>sk_secret</code></div>"
        : "<p>Apps body</p>",
    );
  await mount();
  fireEvent.click(link("Apps"));
  await screen.findByText("sk_secret");
  act(() => api.activate(tabByHref("/dashboard/overview").id));
  await waitFor(() => expect(screen.queryByText("sk_secret")).toBeNull());
  expect(api.state.tabs).toHaveLength(2);
  act(() => api.activate(tabByHref("/dashboard/apps").id));
  await screen.findByText("Apps body");
  expect(calls["/dashboard/apps"]).toBe(2);
  expect(screen.queryByText("sk_secret")).toBeNull();
});

test("beforeunload is armed while any tab is dirty", async () => {
  handlers["/dashboard/overview"] = () =>
    pageHtml(
      "Overview",
      '<form action="/dashboard/x" method="post"><input name="n" value="a" aria-label="n"></form>',
    );
  await mount();
  const probe = () => {
    const e = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(e);
    return e.defaultPrevented;
  };
  expect(probe()).toBe(false);
  fireEvent.input(screen.getByLabelText("n"), { target: { value: "b" } });
  await waitFor(() => expect(probe()).toBe(true));
  // A pinned tab opens the next page in a new tab: no discard prompt, and the
  // hidden tab keeps its edits and the guard.
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  expect(screen.queryByText("Discard unsaved changes?")).toBeNull();
  expect(probe()).toBe(true);
  expect(api.dirtyIds.size).toBe(1);
  expect((screen.getByLabelText("n") as HTMLInputElement).value).toBe("b");
});

test("closing a dirty tab asks first", async () => {
  handlers["/dashboard/payments"] = () =>
    pageHtml(
      "Payments",
      '<form action="/dashboard/x" method="post"><input name="n" value="a" aria-label="n"></form>',
    );
  await mount();
  fireEvent.click(link("Payments"));
  await screen.findByLabelText("n");
  fireEvent.input(screen.getByLabelText("n"), { target: { value: "b" } });
  await waitFor(() => expect(api.dirtyIds.size).toBe(1));
  const id = tabByHref("/dashboard/payments").id;
  act(() => api.close(id));
  await screen.findByText("Discard unsaved changes?");
  expect(api.state.tabs).toHaveLength(2);
  fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
  await waitFor(() =>
    expect(screen.queryByText("Discard unsaved changes?")).toBeNull(),
  );
  act(() => api.close(id));
  fireEvent.click(
    await screen.findByRole("button", { name: "Discard and close" }),
  );
  await waitFor(() => expect(api.state.tabs).toHaveLength(1));
  expect(api.state.activeId).toBe(api.state.tabs[0].id);
  await waitFor(() => expect(api.dirtyIds.size).toBe(0));
});

test("auth page is never persisted as a tab", async () => {
  handlers["/dashboard/overview"] = () => loginHtml;
  await mount();
  expect(document.querySelector(".auth-workspace")).not.toBeNull();
  expect(document.querySelector(".app-shell")).toBeNull();
  expect(
    Object.keys(localStorage).filter((k) => k.startsWith("paymux-tabs:")),
  ).toEqual([]);
});

test("a login URL reached from the shell does not become a tab", async () => {
  handlers["/dashboard/login"] = () => loginHtml;
  const router = await mount();
  await act(() => router.navigate({ to: "/dashboard/login" }));
  await waitFor(() =>
    expect(document.querySelector(".auth-workspace")).not.toBeNull(),
  );
  const saved = localStorage.getItem(storageName)!;
  expect(JSON.parse(saved).tabs.map((t: { href: string }) => t.href)).toEqual([
    "/dashboard/overview",
  ]);
});

test("tabs persist per user and mode with only id, href, title, pinned", async () => {
  await mount();
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  const saved = JSON.parse(localStorage.getItem(storageName)!);
  expect(saved.tabs).toHaveLength(2);
  expect(Object.keys(saved.tabs[1]).sort()).toEqual([
    "href",
    "id",
    "pinned",
    "title",
  ]);
});

test("popstate to an open tab activates it", async () => {
  const router = await mount();
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  act(() => router.history.back());
  await waitFor(() => expect(sectionOf("Overview body")?.hidden).toBe(false));
  expect(api.state.tabs).toHaveLength(2);
  expect(api.state.activeId).toBe(tabByHref("/dashboard/overview").id);
  expect(sectionOf("Payments body")?.hidden).toBe(true);
});

test("popstate to a URL no tab shows loads into the active tab", async () => {
  const router = await mount();
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  fireEvent.click(link("Apps"));
  await screen.findByText("Apps body");
  // Payments was replaced in place by Apps.
  expect(api.state.tabs.map((t) => t.href)).toEqual([
    "/dashboard/overview",
    "/dashboard/apps",
  ]);
  act(() => router.history.back());
  await screen.findByText("Payments body");
  expect(api.state.tabs.map((t) => t.href)).toEqual([
    "/dashboard/overview",
    "/dashboard/payments",
  ]);
  expect(api.state.activeId).toBe(tabByHref("/dashboard/payments").id);
});

test("switching tabs replaces history; in-tab navigation pushes", async () => {
  const router = await mount();
  const push = vi.spyOn(router.history, "push");
  const replace = vi.spyOn(router.history, "replace");
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  expect(push).toHaveBeenCalledTimes(1);
  fireEvent.click(link("Apps"));
  await screen.findByText("Apps body");
  expect(push).toHaveBeenCalledTimes(2);
  expect(replace).not.toHaveBeenCalled();
  act(() => api.activate(tabByHref("/dashboard/overview").id));
  await waitFor(() => expect(replace).toHaveBeenCalledTimes(1));
  expect(push).toHaveBeenCalledTimes(2);
  expect(router.state.location.pathname).toBe("/dashboard/overview");
});

test("openHref(new) adds a tab; closing and reopening restores it", async () => {
  await mount();
  act(() => api.openHref("/dashboard/apps", "new"));
  await screen.findByText("Apps body");
  expect(api.state.tabs).toHaveLength(2);
  const id = tabByHref("/dashboard/apps").id;
  act(() => api.close(id));
  await waitFor(() => expect(api.state.tabs).toHaveLength(1));
  expect(sectionOf("Overview body")?.hidden).toBe(false);
  act(() => api.reopen());
  await screen.findByText("Apps body");
  expect(api.state.tabs).toHaveLength(2);
  act(() => api.pin(api.state.activeId, true));
  act(() => api.move(api.state.activeId, 0));
  expect(api.state.tabs.every((t) => t.pinned)).toBe(true);
});

test("a hidden clean tab unmounts after ten idle minutes", async () => {
  await mount();
  fireEvent.click(link("Payments"));
  await screen.findByText("Payments body");
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  act(() => api.activate(tabByHref("/dashboard/overview").id));
  await act(() => vi.advanceTimersByTimeAsync(9 * 60 * 1000));
  expect(screen.queryByText("Payments body")).not.toBeNull();
  await act(() => vi.advanceTimersByTimeAsync(61 * 1000));
  expect(screen.queryByText("Payments body")).toBeNull();
  // Let the query cache drop the unobserved page (gcTime 0).
  await act(() => vi.advanceTimersByTimeAsync(10));
  act(() => api.activate(tabByHref("/dashboard/payments").id));
  await act(() => vi.advanceTimersByTimeAsync(50));
  expect(screen.queryByText("Payments body")).not.toBeNull();
  expect(calls["/dashboard/payments"]).toBe(2);
});
