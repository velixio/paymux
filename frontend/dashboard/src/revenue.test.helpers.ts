import type { ViewNode } from "./adapter";

export const el = (
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
