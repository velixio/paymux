import { describe, expect, test } from "vitest";
import type { ViewNode } from "./adapter";
import { slugify, splitHead, splitSections } from "./sections";

const el = (
  tag: string,
  text = "",
  attrs: Record<string, string> = {},
): ViewNode => ({
  tag,
  attrs,
  children: text ? [{ tag: "#text", attrs: {}, children: [], text }] : [],
});

describe("slugify", () => {
  test("lowercases, joins words with dashes, trims", () => {
    expect(slugify("Webhook deliveries")).toBe("webhook-deliveries");
    expect(slugify("  Refunds & credits! ")).toBe("refunds-credits");
    expect(slugify("???")).toBe("section");
  });
});

describe("splitSections", () => {
  const record = [
    el("h1", "pay_1"),
    el("p", "intro", { class: "muted" }),
    el("dl"),
    el("h3", "Attempts"),
    el("table"),
    el("h3", "Ledger"),
    el("table"),
    el("h4", "Refund"),
    el("form"),
  ];

  test("the head holds the h1 and what hangs off it; Details is the rest before the first section", () => {
    const s = splitSections(record)!;
    expect(s.head.map((n) => n.tag)).toEqual(["h1", "p"]);
    expect(s.details.map((n) => n.tag)).toEqual(["dl"]);
    expect(s.sections.map((x) => [x.title, x.slug])).toEqual([
      ["Attempts", "attempts"],
      ["Ledger", "ledger"],
    ]);
    expect(s.sections[1].nodes.map((n) => n.tag)).toEqual([
      "table",
      "h4",
      "form",
    ]); // h4 stays inside its section
  });

  test("fewer than two sections: no tabs", () => {
    expect(
      splitSections([el("h1", "x"), el("dl"), el("h3", "Only"), el("table")]),
    ).toBeNull();
    expect(splitSections([el("h1", "x"), el("dl")])).toBeNull();
    expect(splitSections([])).toBeNull();
  });

  test("h2 and h3 both start sections; duplicate and empty titles get unique slugs", () => {
    const s = splitSections([
      el("h2", "Events"),
      el("p", "a"),
      el("h3", "Events"),
      el("p", "b"),
      el("h3"),
      el("p", "c"),
    ])!;
    expect(s.head).toEqual([]);
    expect(s.details).toEqual([]);
    expect(s.sections.map((x) => x.slug)).toEqual([
      "events",
      "events-2",
      "section",
    ]);
    expect(s.sections[2].title).toBe("Section 3");
  });

  test("head: action links and back links directly after the h1 stay with it", () => {
    const s = splitSections([
      el("a", "< Payments"),
      el("h1", "pay_1"),
      el("nav", "", { class: "overview-actions" }),
      el("a", "Open", { class: "btn" }),
      el("dl"),
      el("h3", "A"),
      el("h3", "B"),
    ])!;
    expect(s.head.map((n) => n.tag)).toEqual(["a", "h1", "nav", "a"]);
    expect(s.details.map((n) => n.tag)).toEqual(["dl"]);
  });
});

describe("splitHead", () => {
  test("separates the h1 header from the body, with or without sections", () => {
    const { head, rest } = splitHead([
      el("h1", "x"),
      el("p", "d", { class: "muted" }),
      el("dl"),
      el("table"),
    ]);
    expect(head.map((n) => n.tag)).toEqual(["h1", "p"]);
    expect(rest.map((n) => n.tag)).toEqual(["dl", "table"]);
  });
  test("no h1: everything is the body", () => {
    expect(splitHead([el("dl")]).head).toEqual([]);
    expect(splitHead([]).rest).toEqual([]);
  });
});
