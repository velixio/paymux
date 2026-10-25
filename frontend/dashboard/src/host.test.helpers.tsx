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

export function pageHtml(
  title: string,
  body: string,
  opts: { nav?: string; warnings?: string[]; dashboardTheme?: string } = {},
) {
  const banners = (opts.warnings ?? [])
    .map((w) => `<div class="dev-banner">${w}</div>`)
    .join("");
  const root = opts.dashboardTheme
    ? `<div id="paymux-react-root" data-dashboard-theme="${opts.dashboardTheme}"></div>`
    : "";
  return `<title>${title} - Paymux</title>${root}<div id="paymux-server-page"><header class="dash-head"><span class="muted">u@example.com (admin)</span><form class="modeform" method="post" action="/dashboard/mode"><span class="mode">live mode</span><input type="hidden" name="csrf" value="tok"><button name="mode" value="test">Switch to test</button></form></header>${banners}<nav class="dash-nav">${opts.nav ?? NAV}</nav><section class="dash-main">${body}</section></div>`;
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
      const path = new URL(input, location.origin).pathname;
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

// A realistic Overview revenue section (see web/templates/dash_overview.html).
export function revenueSectionHtml(
  opts: {
    changes?: [string, string, string];
    rows?: [string, number, number, number, number][];
    app?: string;
  } = {},
) {
  const [gc, rc, nc] = opts.changes ?? ["+54.7%", "+25.0%", ""];
  const rows = opts.rows ?? [
    ["2026-10-01T00:00:00+05:30", 1500000, 50000, 1000000, 0],
    ["2026-10-02T00:00:00+05:30", 899175, 0, 500000, 0],
  ];
  const tr = rows
    .map(
      ([s, g, r, pg, pr]) =>
        `<tr data-start="${s}" data-gross="${g}" data-refunds="${r}" data-prev-gross="${pg}" data-prev-refunds="${pr}"><td>${s.slice(0, 10)}</td><td>x</td><td>x</td><td>x</td></tr>`,
    )
    .join("");
  const sel = (v: string) => (opts.app === v ? " selected" : "");
  return `<section id="revenue" aria-label="Revenue"><h2>Revenue</h2>
<nav class="range-presets" aria-label="Revenue range"><a href="/dashboard/overview?range=7d">7D</a><a href="/dashboard/overview?range=30d" aria-current="page" class="active">30D</a><a href="/dashboard/overview?range=12m">12M</a></nav>
<form class="revenue-filters" method="get" action="/dashboard/overview">
<label>From <input type="date" name="from" value="2026-09-26"></label>
<label>To <input type="date" name="to" value="2026-10-25"></label>
<label>App <select name="app"><option value=""${sel("")}>All apps</option><option value="app_1"${sel("app_1")}>Demo</option></select></label>
<label>Currency <select name="currency"><option value="">Default</option><option value="USD">USD</option><option value="INR">INR</option></select></label>
<label>Group by <select name="bucket"><option value="auto" selected>Auto</option><option value="day">Day</option><option value="month">Month</option></select></label>
<input type="hidden" name="range" value="custom"><button class="btn small" type="submit">Apply custom range</button></form>
<dl class="revenue-summary"><div><dt>Gross</dt><dd>23991.75 INR <span class="change" data-metric="gross">${gc}</span></dd></div><div><dt>Refunds</dt><dd>500.00 INR <span class="change" data-metric="refunds">${rc}</span></dd></div><div><dt>Net</dt><dd>23491.75 INR <span class="change" data-metric="net">${nc}</span></dd></div></dl>
<p class="muted">Foreign-currency amounts are converted to INR at the stored daily rate. Includes estimated conversions (no rate stored for some days).</p>
<div class="table-scroll"><table id="revenue-data" class="grid" data-currency="INR" data-exp="2" data-bucket="day" data-converted="true" data-estimated="true"><thead><tr><th>Period</th><th>Gross</th><th>Refunds</th><th>Net</th></tr></thead><tbody>${tr}</tbody></table></div>
<p><a href="/dashboard/overview/revenue.csv?range=30d" download>Download CSV</a></p></section>`;
}
