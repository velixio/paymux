import { describe, expect, test } from "vitest";
import { compactMoney, parseRevenue, scale } from "./revenue";
import { isDownloadPath, type ViewNode } from "./adapter";

const el = (
  tag: string,
  attrs: Record<string, string> = {},
  children: ViewNode[] = [],
): ViewNode => ({ tag, attrs, children });

export function revenueTable(
  rows: [string, number, number, number, number][],
  extra: Record<string, string> = {},
): ViewNode {
  return el(
    "table",
    {
      id: "revenue-data",
      "data-currency": "INR",
      "data-exp": "2",
      "data-bucket": "day",
      "data-converted": "false",
      "data-estimated": "false",
      ...extra,
    },
    [
      el("thead"),
      el(
        "tbody",
        {},
        rows.map(([start, g, r, pg, pr]) =>
          el("tr", {
            "data-start": start,
            "data-gross": String(g),
            "data-refunds": String(r),
            "data-prev-gross": String(pg),
            "data-prev-refunds": String(pr),
          }),
        ),
      ),
    ],
  );
}

describe("parseRevenue", () => {
  test("parseRevenue reads data attributes", () => {
    const s = parseRevenue(
      revenueTable(
        [
          ["2026-10-01T00:00:00+05:30", 100000, 25000, 80000, 5000],
          ["2026-10-02T00:00:00+05:30", 0, 0, 0, 0],
        ],
        {
          "data-bucket": "month",
          "data-converted": "true",
          "data-estimated": "true",
        },
      ),
    )!;
    expect(s).toMatchObject({
      currency: "INR",
      exp: 2,
      bucket: "month",
      converted: true,
      estimated: true,
    });
    expect(s.points).toEqual([
      {
        start: "2026-10-01T00:00:00+05:30",
        gross: 100000,
        refunds: 25000,
        net: 75000,
        prevNet: 75000,
      },
      {
        start: "2026-10-02T00:00:00+05:30",
        gross: 0,
        refunds: 0,
        net: 0,
        prevNet: 0,
      },
    ]);
  });

  test("parseRevenue returns null without rows", () => {
    expect(parseRevenue(revenueTable([]))).toBeNull();
    expect(
      parseRevenue(el("table", { id: "revenue-data" }, [el("tr", {}, [])])),
    ).toBeNull();
  });

  test("parseRevenue tolerates missing or malformed numbers", () => {
    const s = parseRevenue(
      el("table", { id: "revenue-data" }, [
        el("tr", { "data-start": "2026-10-01T00:00:00Z", "data-gross": "x" }),
      ]),
    )!;
    expect(s.currency).toBe("");
    expect(s.exp).toBe(2);
    expect(s.points[0]).toMatchObject({
      gross: 0,
      refunds: 0,
      net: 0,
      prevNet: 0,
    });
  });
});

describe("scale", () => {
  test("scale handles all-zero series", () => {
    const s = scale([0, 0, 0], 200);
    expect(s.max).toBeGreaterThan(s.min);
    expect(Number.isNaN(s.y(0))).toBe(false);
    expect(s.ticks).toContain(0);
  });

  test("scale includes negative net", () => {
    const s = scale([-5000, 12000, 3000], 200);
    expect(s.min).toBeLessThan(0);
    expect(s.ticks).toContain(0);
    expect(s.y(s.max)).toBe(0);
    expect(s.y(s.min)).toBe(200);
    expect(s.y(0)).toBeGreaterThan(0);
    expect(s.y(0)).toBeLessThan(200);
  });

  test("scale picks nice ticks covering the data", () => {
    const s = scale([0, 4823100], 200);
    expect(s.ticks[0]).toBe(0);
    expect(s.ticks.at(-1)!).toBeGreaterThanOrEqual(4823100);
    const steps = new Set(s.ticks.slice(1).map((t, i) => t - s.ticks[i]));
    expect(steps.size).toBe(1);
    expect(s.ticks.length).toBeLessThanOrEqual(8);
  });
});

describe("compactMoney", () => {
  test.each([
    [95000, "INR", 2, "₹950"],
    [4823100, "INR", 2, "₹48.2K"],
    [48231000, "INR", 2, "₹4.8 L"],
    [1200000000, "INR", 2, "₹1.2 Cr"],
    [-4823100, "INR", 2, "-₹48.2K"],
    [0, "INR", 2, "₹0"],
    [123456789, "USD", 2, "$1.2M"],
    [950, "JPY", 0, "¥950"],
  ])("compactMoney %d %s", (minor, currency, exp, want) => {
    expect(compactMoney(minor, currency, exp)).toBe(want);
  });
});

test("the revenue CSV is a native download, not an in-app page", () => {
  expect(isDownloadPath("/dashboard/overview/revenue.csv")).toBe(true);
});
