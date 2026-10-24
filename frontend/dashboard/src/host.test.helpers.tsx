import { expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider, createMemoryHistory } from "@tanstack/react-router";
import { createDashboardRouter, useTabs, type TabsApi } from "./shell";

// Shared harness for the shell tests: a stubbed fetch serving fixture pages.
const NAV = ["overview", "payments", "apps"]
  .map(
    (p) => `<a href="/dashboard/${p}">${p[0].toUpperCase() + p.slice(1)}</a>`,
  )
  .join("");

export function pageHtml(title: string, body: string) {
  return `<title>${title} - Paymux</title><div id="paymux-server-page"><header class="dash-head"><span class="muted">u@example.com (admin)</span><form class="modeform" method="post" action="/dashboard/mode"><span class="mode">live mode</span></form></header><nav class="dash-nav">${NAV}</nav><section class="dash-main">${body}</section></div>`;
}
export const loginHtml =
  '<title>Sign in - Paymux</title><div id="paymux-server-page"><form action="/dashboard/login"><input name="csrf" value="t"></form></div>';
export const storageName = "paymux-tabs:u@example.com (admin):live mode";

type Handler = (call: number) => string;
export let handlers: Record<string, Handler>;
export let calls: Record<string, number>;

export function resetHost() {
  localStorage.clear();
  calls = {};
  handlers = {
    "/dashboard/overview": () => pageHtml("Overview", "<p>Overview body</p>"),
    "/dashboard/payments": () => pageHtml("Payments", "<p>Payments body</p>"),
    "/dashboard/apps": () => pageHtml("Apps", "<p>Apps body</p>"),
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string) => {
      const path = input.split("?")[0];
      calls[path] = (calls[path] ?? 0) + 1;
      const html = handlers[path]?.(calls[path]) ?? "";
      return {
        ok: true,
        status: 200,
        redirected: false,
        url: location.origin + input,
        headers: { get: () => "text/html" },
        text: async () => html,
      };
    }),
  );
}

export let api: TabsApi;
function Probe() {
  api = useTabs();
  return null;
}

export async function mount(initial = "/dashboard/overview") {
  const client = new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 30_000,
        gcTime: 0,
        retry: false,
        refetchOnWindowFocus: false,
      },
    },
  });
  const router = createDashboardRouter(
    createMemoryHistory({ initialEntries: [initial] }),
    <Probe />,
  );
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  await waitFor(() =>
    expect(
      document.querySelector("[data-tab], .auth-workspace"),
    ).not.toBeNull(),
  );
  return router;
}

export const link = (name: string) =>
  screen.getAllByRole("link", { name }).find((a) => a.closest("aside"))!;
export const sectionOf = (text: string) =>
  screen.queryByText(text)?.closest("section") ?? null;
export const tabByHref = (href: string) =>
  api.state.tabs.find((t) => t.href === href)!;
