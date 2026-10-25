import { afterAll, describe, expect, test } from "vitest";
import { Window } from "happy-dom";
import {
  parseDashboard,
  isDownloadPath,
  attributes,
  preservePostedValues,
  type ViewNode,
} from "./adapter";
const win = new Window({ url: "http://localhost:18080/dashboard/settings" });
Object.assign(globalThis, {
  Node: win.Node,
  Element: win.Element,
  DOMParser: win.DOMParser,
  location: win.location,
});
afterAll(() => win.happyDOM.abort());
const html = `<title>Settings - Paymux</title><div id="paymux-server-page"><header class="dash-head"><span class="muted">admin@example.com (admin)</span><form class="modeform" method="post" action="/dashboard/mode"><span class="mode">test mode</span><input name="csrf" value="csrf-test"><button name="mode" value="live">Switch to live</button></form><form method="post" action="/dashboard/logout"><input name="csrf" value="csrf-test"></form></header><nav class="dash-nav"><a href="/dashboard/settings" aria-current="page">Settings</a></nav><section class="dash-main"><script>alert('never execute')</script><form action="/dashboard/settings/dunning_schedule" method="post"><input name="csrf" value="csrf-test"><input name="value" value="24,72"></form><form action="/dashboard/settings/predebit_email" method="post"><input type="hidden" name="value" value="false"><input type="checkbox" name="value" value="true" checked></form><form action="/dashboard/apps/app_1/endpoints" method="post"><input name="url" value="https://example.com"></form></section></div>`;
function page() {
  return parseDashboard(
    new win.DOMParser().parseFromString(
      html,
      "text/html",
    ) as unknown as Document,
    "/dashboard/settings",
  );
}
describe("existing Go page contract", () => {
  test("retains session actions, CSRF, mode and permission-derived navigation", () => {
    const p = page();
    expect(p.auth).toBe(false);
    expect(p.mode).toBe("test mode");
    expect(p.nav).toEqual([
      { href: "/dashboard/settings", label: "Settings", active: true },
    ]);
    expect(JSON.stringify(p.modeForm)).toContain("csrf-test");
    expect(JSON.stringify(p.modeForm)).toContain("Switch to live");
    expect(p.content.some((n) => n.tag === "script")).toBe(false);
  });
  test("server validation response retains entered values in only the submitted form", () => {
    const data = new FormData();
    data.set("value", "invalid retry schedule");
    const p = preservePostedValues(
      page(),
      "/dashboard/settings/dunning_schedule",
      data,
    );
    expect(
      p.content.find((n) => n.attrs.action?.endsWith("/dunning_schedule"))!
        .children[1].attrs.value,
    ).toBe("invalid retry schedule");
    expect(
      p.content.find((n) => n.attrs.action?.endsWith("/endpoints"))!.children[0]
        .attrs.value,
    ).toBe("https://example.com");
  });
  test("an unchecked boolean remains unchecked after a rejected save", () => {
    const data = new FormData();
    data.set("value", "false");
    const p = preservePostedValues(
      page(),
      "/dashboard/settings/predebit_email",
      data,
    );
    expect(
      p.content.find((n) => n.attrs.action?.endsWith("/predebit_email"))!
        .children[1].attrs.checked,
    ).toBeUndefined();
  });
  test("React props preserve native validation, multipart uploads and labels", () => {
    const n: ViewNode = {
      tag: "input",
      attrs: {
        type: "file",
        name: "evidence",
        "aria-label": "Evidence",
        required: "",
        enctype: "multipart/form-data",
        readonly: "",
      },
      children: [],
    };
    expect(attributes(n)).toMatchObject({
      type: "file",
      name: "evidence",
      "aria-label": "Evidence",
      required: true,
      encType: "multipart/form-data",
      readOnly: true,
    });
  });
  test("reads a nav count only when the server attached one", () => {
    const d = new win.DOMParser().parseFromString(
      '<title>Keys - Paymux</title><div id="paymux-server-page"><header class="dash-head"></header><nav class="dash-nav"><a href="/dashboard/disputes" data-count="2">Disputes</a><a href="/dashboard/apps" data-count="">Apps</a><a href="/dashboard/logs" data-count="x">Logs</a><a href="/dashboard/audit">Audit</a></nav><section class="dash-main"></section></div>',
      "text/html",
    );
    const p = parseDashboard(d as unknown as Document, "/dashboard/disputes");
    expect(p.nav.map((n) => n.count)).toEqual([
      2,
      undefined,
      undefined,
      undefined,
    ]);
  });
  test("marks pages that show a one-time secret", () => {
    expect(page().once).toBe(false);
    const d = new win.DOMParser().parseFromString(
      '<title>Keys - Paymux</title><div id="paymux-server-page"><header class="dash-head"></header><div data-once><code>sk_once</code></div></div>',
      "text/html",
    );
    expect(
      parseDashboard(d as unknown as Document, "/dashboard/apps").once,
    ).toBe(true);
  });
  test("parses authentication pages without inventing account data", () => {
    const d = new win.DOMParser().parseFromString(
      '<title>Sign in</title><div id="paymux-server-page"><form action="/dashboard/login"><input name="csrf" value="login-token"></form></div>',
      "text/html",
    );
    const p = parseDashboard(d as unknown as Document, "/dashboard/login");
    expect(p.auth).toBe(true);
    expect(p.nav).toEqual([]);
    expect(JSON.stringify(p.content)).toContain("login-token");
  });
});

test("binary and export endpoints keep native downloads", () => {
  for (const path of [
    "/dashboard/invoices/inv_1/pdf",
    "/dashboard/reports/zip",
    "/dashboard/reports/file",
    "/dashboard/invoices/inv_1/einvoice.json",
    "/dashboard/disputes/d_1/evidence/b_1",
  ])
    expect(isDownloadPath(path)).toBe(true);
  expect(isDownloadPath("/dashboard/reports")).toBe(false);
  expect(isDownloadPath("/dashboard/payments/pay_1")).toBe(false);
});
