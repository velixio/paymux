import { afterEach, expect, test } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  within,
} from "@testing-library/react";
import RevenueChart from "./revenue-chart";
import { parseRevenue } from "./revenue";
import { revenueTable } from "./revenue.test.helpers";

afterEach(cleanup);

const day = (n: number) =>
  `2026-10-${String(n).padStart(2, "0")}T00:00:00+05:30`;
const rows = (n: number) =>
  Array.from({ length: n }, (_, i) => [
    day(i + 1),
    (i + 1) * 100000,
    i % 3 === 0 ? 20000 : 0,
    (i + 1) * 50000,
    0,
  ]) as [string, number, number, number, number][];

function chart(n = 5, extra: Record<string, string> = {}) {
  const series = parseRevenue(revenueTable(rows(n), extra))!;
  return render(<RevenueChart series={series} />);
}

test("renders one focusable bar per point with an accessible label", () => {
  chart(5);
  const bars = screen.getAllByRole("img");
  expect(bars).toHaveLength(5);
  expect(bars[0].getAttribute("tabindex")).toBe("0");
  expect(bars[0].getAttribute("aria-label")).toMatch(/1 Oct.*net.*₹800\.00/i);
  expect(document.querySelector("svg")!.innerHTML).not.toContain("NaN");
});

test("hover and focus show a tooltip with exact values", () => {
  chart(5);
  const bars = screen.getAllByRole("img");
  expect(screen.queryByRole("tooltip")).toBeNull();
  fireEvent.mouseEnter(bars[1]);
  const tip = screen.getByRole("tooltip");
  expect(within(tip).getByText(/Gross/)).toBeTruthy();
  expect(tip.textContent).toContain("₹2,000.00");
  fireEvent.mouseLeave(bars[1]);
  expect(screen.queryByRole("tooltip")).toBeNull();
  fireEvent.focus(bars[0]);
  expect(screen.getByRole("tooltip").textContent).toMatch(/Refunds.*₹200\.00/);
  expect(screen.getByRole("tooltip").textContent).toMatch(/Previous/);
  fireEvent.blur(bars[0]);
  expect(screen.queryByRole("tooltip")).toBeNull();
});

test("left and right arrows move focus between bars", () => {
  chart(5);
  const bars = screen.getAllByRole("img");
  bars[0].focus();
  fireEvent.keyDown(bars[0], { key: "ArrowRight" });
  expect(document.activeElement).toBe(bars[1]);
  fireEvent.keyDown(bars[1], { key: "ArrowLeft" });
  expect(document.activeElement).toBe(bars[0]);
  fireEvent.keyDown(bars[0], { key: "ArrowLeft" });
  expect(document.activeElement).toBe(bars[0]);
  fireEvent.keyDown(bars[4], { key: "ArrowRight" });
  fireEvent.keyDown(bars[0], { key: "Enter" });
  expect(document.activeElement).toBe(bars[0]);
});

test("x labels are thinned to at most eight", () => {
  chart(30);
  const labels = document.querySelectorAll("[data-x-label]");
  expect(labels.length).toBeGreaterThan(0);
  expect(labels.length).toBeLessThanOrEqual(8);
});

test("the legend leaves the estimated-conversion note to the server text", () => {
  chart(3, { "data-estimated": "true", "data-converted": "true" });
  expect(screen.queryByText("Includes estimated conversions")).toBeNull();
});

test("monthly, weekly, hourly and quarterly buckets get matching labels", () => {
  for (const [bucket, start, want] of [
    ["month", "2026-03-01T00:00:00+05:30", /Mar/],
    ["hour", "2026-10-05T14:00:00+05:30", /14:00/],
    ["quarter", "2026-10-01T00:00:00+05:30", /Q4/],
    ["week", "2026-10-05T00:00:00+05:30", /5 Oct/],
  ] as const) {
    const series = parseRevenue(
      revenueTable([[start, 1000, 0, 0, 0]], { "data-bucket": bucket }),
    )!;
    const { unmount } = render(<RevenueChart series={series} />);
    expect(screen.getByRole("img").getAttribute("aria-label")).toMatch(want);
    unmount();
  }
});

test("a negative net draws without NaN and keeps zero on the axis", () => {
  const series = parseRevenue(
    revenueTable([
      [day(1), 1000, 5000, 0, 0],
      [day(2), 9000, 0, 0, 0],
    ]),
  )!;
  render(<RevenueChart series={series} />);
  expect(document.querySelector("svg")!.innerHTML).not.toContain("NaN");
  expect(document.querySelector("[data-zero-line]")).not.toBeNull();
});

test("an all-zero series renders", () => {
  const series = parseRevenue(revenueTable([[day(1), 0, 0, 0, 0]]))!;
  render(<RevenueChart series={series} />);
  expect(document.querySelector("svg")!.innerHTML).not.toContain("NaN");
});
