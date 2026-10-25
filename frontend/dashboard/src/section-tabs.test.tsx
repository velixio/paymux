import { afterEach, beforeEach, expect, test } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import type { ViewNode } from "./adapter";
import { PageContent } from "./render";

const text = (t: string): ViewNode => ({
  tag: "#text",
  attrs: {},
  children: [],
  text: t,
});
const el = (tag: string, t = ""): ViewNode => ({
  tag,
  attrs: {},
  children: t ? [text(t)] : [],
});
const nodes = [
  el("h1", "pay_1"),
  el("p", "Details body"),
  el("h3", "Attempts"),
  el("p", "Attempts body"),
  el("h3", "Ledger"),
  el("p", "Ledger body"),
];

beforeEach(() => history.replaceState(null, "", "/dashboard/payments/pay_1"));
afterEach(cleanup);

test("a record page gets Details plus one tab per section; others show only the selected panel", () => {
  render(<PageContent nodes={nodes} href="/dashboard/payments/pay_1" />);
  expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual([
    "Details",
    "Attempts",
    "Ledger",
  ]);
  expect(
    screen.getByRole("tab", { name: "Details" }).getAttribute("aria-selected"),
  ).toBe("true");
  expect(screen.getByText("Details body").closest("[hidden]")).toBeNull();
  expect(
    screen.getByText("Attempts body", { selector: "p" }).closest("[hidden]"),
  ).not.toBeNull();
});

test("selecting a tab shows its section and records #tab-<slug> in the URL", async () => {
  render(
    <PageContent nodes={nodes} href="/dashboard/payments/pay_1" syncHash />,
  );
  fireEvent.mouseDown(screen.getByRole("tab", { name: "Ledger" }));
  fireEvent.click(screen.getByRole("tab", { name: "Ledger" }));
  expect(location.hash).toBe("#tab-ledger");
  expect(screen.getByText("Ledger body").closest("[hidden]")).toBeNull();
  expect(screen.getByText("Details body").closest("[hidden]")).not.toBeNull();
});

test("a #tab-<slug> hash selects that tab on load; an unknown hash falls back to Details", () => {
  history.replaceState(null, "", "/dashboard/payments/pay_1#tab-attempts");
  render(
    <PageContent nodes={nodes} href="/dashboard/payments/pay_1" syncHash />,
  );
  expect(
    screen.getByRole("tab", { name: "Attempts" }).getAttribute("aria-selected"),
  ).toBe("true");
  cleanup();
  history.replaceState(null, "", "/dashboard/payments/pay_1#tab-nope");
  render(
    <PageContent nodes={nodes} href="/dashboard/payments/pay_1" syncHash />,
  );
  expect(
    screen.getByRole("tab", { name: "Details" }).getAttribute("aria-selected"),
  ).toBe("true");
});

test("without syncHash the URL is left alone", () => {
  render(<PageContent nodes={nodes} href="/dashboard/payments/pay_1" />);
  fireEvent.mouseDown(screen.getByRole("tab", { name: "Ledger" }));
  fireEvent.click(screen.getByRole("tab", { name: "Ledger" }));
  expect(location.hash).toBe("");
});

test("list pages and pages with fewer than two sections render as before", () => {
  render(<PageContent nodes={nodes} href="/dashboard/payments" />);
  expect(screen.queryByRole("tab")).toBeNull();
  cleanup();
  render(
    <PageContent nodes={nodes.slice(0, 4)} href="/dashboard/payments/pay_1" />,
  );
  expect(screen.queryByRole("tab")).toBeNull();
  cleanup();
  render(<PageContent nodes={nodes} />);
  expect(screen.queryByRole("tab")).toBeNull();
});
