import { describe, expect, test } from "vitest";
import type { ViewNode } from "./adapter";
import { formatDateText, formatMoneyText, polishNodes } from "./polish";

const text = (t: string): ViewNode => ({
  tag: "#text",
  attrs: {},
  children: [],
  text: t,
});
const el = (
  tag: string,
  children: ViewNode[] = [],
  attrs: Record<string, string> = {},
): ViewNode => ({ tag, attrs, children });
const table = (heads: string[], rows: (string | ViewNode)[][]) =>
  el("table", [
    el("thead", [
      el(
        "tr",
        heads.map((h) => el("th", [text(h)])),
      ),
    ]),
    el(
      "tbody",
      rows.map((r) =>
        el(
          "tr",
          r.map((c) => el("td", [typeof c === "string" ? text(c) : c])),
        ),
      ),
    ),
  ]);
const cell = (t: ViewNode, row: number, col: number) =>
  t.children[1].children[row].children[col];

describe("money", () => {
  test("INR uses the rupee sign and Indian grouping", () => {
    expect(formatMoneyText("2750.00 INR")).toBe("₹2,750.00");
    expect(formatMoneyText("1234567.50 INR")).toBe("₹12,34,567.50");
  });
  test("other currencies keep their symbol and decimals", () => {
    expect(formatMoneyText("99.00 USD")).toBe("$99.00");
    expect(formatMoneyText("-120.00 EUR")).toBe("-€120.00");
    expect(formatMoneyText("500 JPY")).toBe("¥500");
  });
  test("anything else is left alone", () => {
    for (const s of ["2750", "INR", "2750.00 inr", "abc 12.00 INR", ""])
      expect(formatMoneyText(s), s).toBeNull();
  });
});

describe("dates", () => {
  test("date and time read as words, without changing the time zone", () => {
    expect(formatDateText("2026-10-25 08:46:28")).toBe("25 Oct 2026, 08:46");
    expect(formatDateText("2026-01-05 23:59")).toBe("5 Jan 2026, 23:59");
    expect(formatDateText("2026-10-25")).toBe("25 Oct 2026");
  });
  test("anything else is left alone", () => {
    for (const s of ["2026-10", "25/10/2026", "2026-13-01", "today", ""])
      expect(formatDateText(s), s).toBeNull();
  });
});

describe("polishNodes", () => {
  test("money and date cells get readable text, the original in a title", () => {
    const t = table(
      ["Amount", "Created"],
      [["2750.00 INR", "2026-10-25 08:46:28"]],
    );
    const [out] = polishNodes([t]);
    const amount = cell(out, 0, 0).children[0];
    expect(amount.tag).toBe("span");
    expect(amount.attrs.class).toBe("money");
    expect(amount.attrs.title).toBe("2750.00 INR");
    expect(amount.children[0].text).toBe("₹2,750.00");
    const when = cell(out, 0, 1).children[0];
    expect(when.attrs.class).toBe("when");
    expect(when.attrs.title).toBe("2026-10-25 08:46:28");
    expect(when.children[0].text).toBe("25 Oct 2026, 08:46");
  });

  test("plain words in a status-like column become status pills", () => {
    const t = table(
      ["Email", "Role", "Status", "2FA", "Level", "State"],
      [["a@example.com", "admin", "active", "pending", "error", "to_link"]],
    );
    const [out] = polishNodes([t]);
    for (const [col, word] of [
      [2, "active"],
      [3, "pending"],
      [4, "error"],
      [5, "to_link"],
    ] as const) {
      const pill = cell(out, 0, col).children[0];
      expect(pill.tag, word).toBe("span");
      expect(pill.attrs.class).toBe(`status status-${word}`);
      expect(pill.children[0].text).toBe(word);
    }
    // Other columns are untouched.
    expect(cell(out, 0, 1).children[0].tag).toBe("#text");
  });

  test("a status cell that already holds markup or long text is left alone", () => {
    const pill = el("span", [text("active")], { class: "status" });
    const t = table(
      ["Status"],
      [[pill], ["failed: the provider timed out after 30 seconds"]],
    );
    const [out] = polishNodes([t]);
    expect(cell(out, 0, 0).children[0]).toBe(pill);
    expect(cell(out, 1, 0).children[0].tag).toBe("#text");
  });

  test("a lone 'nothing here' row is marked as the table's empty state", () => {
    const empty = el("table", [
      el("thead", [el("tr", [el("th", [text("A")]), el("th", [text("B")])])]),
      el("tbody", [
        el("tr", [
          el("td", [text("No alerts.")], { colspan: "2", class: "muted" }),
        ]),
      ]),
    ]);
    const [out] = polishNodes([empty]);
    expect(out.children[1].children[0].attrs.class).toBe("empty-row");
  });

  test("description lists format their values too", () => {
    const dl = el("dl", [
      el("dt", [text("Amount")]),
      el("dd", [text("10.00 USD")]),
    ]);
    const [out] = polishNodes([dl]);
    expect(out.children[1].children[0].attrs.class).toBe("money");
  });
});
