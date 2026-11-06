import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import type { ViewNode } from "./adapter";
import { PageContent } from "./render";
import { recordSplit } from "./section-tabs";

// RenderNode only needs the runtime for forms and links; these pages have none.
vi.mock("./runtime", async (original) => ({
  ...(await original<typeof import("./runtime")>()),
  useRuntime: () => ({
    submit() {},
    pending: false,
    markDirty() {},
    hasEdits: false,
    copy: async () => {},
  }),
}));

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
  fireEvent.mouseDown(screen.getByRole("tab", { name: "Details" }));
  fireEvent.click(screen.getByRole("tab", { name: "Details" }));
  expect(location.hash).toBe("");
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

test("webhook editing stays visible inside existing app section navigation", () => {
  const appNodes = [
    el("h1", "My app"),
    {
      tag: "nav",
      attrs: { class: "tabs", "aria-label": "App sections" },
      children: [],
    },
    el("p", "Webhook list"),
  ];
  const page = render(
    <PageContent nodes={appNodes} href="/dashboard/apps/app_1?tab=webhooks" />,
  );
  page.rerender(
    <PageContent
      nodes={[
        ...appNodes,
        el("h3", "Edit webhook"),
        {
          tag: "label",
          attrs: {},
          children: [
            text("Endpoint URL"),
            {
              tag: "input",
              attrs: {
                name: "url",
                value: "https://merchant.example/webhooks",
              },
              children: [],
            },
          ],
        },
        el("h3", "Deliveries"),
        el("p", "Delivery history"),
      ]}
      href="/dashboard/apps/app_1?tab=webhooks&endpoint=whe_1"
    />,
  );
  expect(screen.queryByRole("tab", { name: "Details" })).toBeNull();
  expect(
    screen.getByRole("textbox", { name: "Endpoint URL" }).closest("[hidden]"),
  ).toBeNull();
  expect(
    screen.getByRole("heading", { name: "Edit webhook" }).closest("[hidden]"),
  ).toBeNull();
  expect(screen.getByText("Delivery history").closest("[hidden]")).toBeNull();
});

test("recordSplit returns null for the timeline page", () => {
  const timeline: ViewNode = {
    tag: "section",
    attrs: { class: "payment-timeline" },
    children: [el("h2", "Timeline"), el("h2", "Risk")],
  };
  expect(recordSplit([el("h1", "pay_1"), timeline, ...nodes.slice(2)])).toBeNull();
  expect(recordSplit(nodes)).not.toBeNull();
});
