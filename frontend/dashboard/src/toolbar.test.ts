import { describe, expect, test } from "vitest";
import { textOf, type ViewNode } from "./adapter";
import { extractToolbar } from "./toolbar";

const t = (text: string): ViewNode => ({
  tag: "#text",
  attrs: {},
  children: [],
  text,
});
const el = (
  tag: string,
  attrs: Record<string, string> = {},
  ...children: (ViewNode | string)[]
): ViewNode => ({
  tag,
  attrs,
  children: children.map((c) => (typeof c === "string" ? t(c) : c)),
});
const a = (href: string, label: string, cls = "") =>
  el("a", cls ? { href, class: cls } : { href }, label);
const mode = el("span", { class: "mode mode-live" }, "live");
const h1 = (...kids: (ViewNode | string)[]) => el("h1", {}, ...kids);
const filters = el(
  "form",
  { class: "filters", method: "get", action: "/dashboard/payments" },
  el(
    "select",
    { name: "status" },
    el("option", { value: "" }, "Any status"),
    el("option", { value: "succeeded", selected: "" }, "succeeded"),
    el("option", { value: "failed" }, "failed"),
  ),
);

describe("extractToolbar", () => {
  test("title drops the mode span but keeps status pills and muted small text", () => {
    const status = el("span", { class: "status status-paid" }, "paid");
    const r = extractToolbar([
      h1("pay_1 ", status, " ", mode, el("small", { class: "muted" }, "x")),
    ])!;
    const tags = r.toolbar.title.map((n) => n.tag);
    expect(tags).toContain("span");
    expect(r.toolbar.title.find((n) => n.tag === "span")).toBe(status);
    expect(r.toolbar.title.some((n) => n.attrs.class?.includes("mode"))).toBe(
      false,
    );
    expect(r.toolbar.title.map(textOf).join("").trim()).toContain("pay_1");
    expect(r.rest).toEqual([]);
  });

  test("the first p.muted right after the title becomes the description", () => {
    const desc = el("p", { class: "muted" }, "Track each payment.");
    const other = el("p", {}, "Body");
    const r = extractToolbar([h1("Payments ", mode), desc, other])!;
    expect(r.toolbar.description).toBe("Track each payment.");
    expect(r.rest).toEqual([other]);
  });

  test("a muted paragraph that is not directly after the title stays content", () => {
    const body = el("section", {});
    const muted = el("p", { class: "muted" }, "later");
    const r = extractToolbar([h1("Apps"), body, muted])!;
    expect(r.toolbar.description).toBeNull();
    expect(r.rest).toEqual([body, muted]);
  });

  test("actions come from nav.overview-actions and a.btn after the title", () => {
    const view = a("/dashboard/payments", "View payments", "btn");
    const manage = a("/dashboard/providers", "Manage");
    const nav = el("nav", { class: "overview-actions" }, view, manage);
    const r = extractToolbar([
      h1("Overview"),
      el("p", { class: "muted" }, "d"),
      nav,
      el("section", {}),
    ])!;
    expect(r.toolbar.actions).toEqual([view, manage]);
    expect(r.rest).toHaveLength(1);

    const solo = a("/dashboard/links/new", "New link", "btn");
    const r2 = extractToolbar([
      h1("Links"),
      el("p", {}, solo),
      el("table", {}),
    ])!;
    expect(r2.toolbar.actions).toEqual([solo]);
    expect(r2.rest).toHaveLength(1);
  });

  test("a leading back link becomes the back button", () => {
    const r = extractToolbar([
      el("p", {}, a("/dashboard/payments", "← Payments")),
      h1("pay_1"),
      el("dl", {}),
    ])!;
    expect(r.toolbar.back).toEqual({
      href: "/dashboard/payments",
      label: "Payments",
    });
    expect(r.rest).toHaveLength(1);
  });

  test("a GET filter form with a status select supplies the chips", () => {
    const r = extractToolbar([h1("Payments", mode), filters, el("table", {})])!;
    expect(r.toolbar.statuses).toEqual({
      options: [
        { value: "", label: "All", selected: false },
        { value: "succeeded", label: "succeeded", selected: true },
        { value: "failed", label: "failed", selected: false },
      ],
    });
    // The form itself stays in the page.
    expect(r.rest[0]).toBe(filters);
  });

  test("no chips for POST forms or forms without a status select", () => {
    const post = el(
      "form",
      { class: "filters", method: "post" },
      filters.children[0],
    );
    expect(extractToolbar([h1("X"), post])!.toolbar.statuses).toBeNull();
    const plain = el(
      "form",
      { class: "filters", method: "get" },
      el("input", { name: "q" }),
    );
    expect(extractToolbar([h1("X"), plain])!.toolbar.statuses).toBeNull();
  });

  test("blank text nodes between parts are ignored", () => {
    const r = extractToolbar([
      t("\n"),
      h1("Alerts"),
      t("\n"),
      el("p", { class: "muted" }, "d"),
      t("\n"),
      el("section", {}),
    ])!;
    expect(r.toolbar.description).toBe("d");
    expect(r.rest.filter((n) => n.tag !== "#text")).toHaveLength(1);
  });

  test("pages that do not start with a title get no toolbar", () => {
    expect(
      extractToolbar([el("p", { class: "notice" }, "hi"), h1("X")]),
    ).toBeNull();
    expect(extractToolbar([])).toBeNull();
  });
});
