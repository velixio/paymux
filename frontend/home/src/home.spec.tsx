import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import Home from "./home";
import Hosting from "./hosting";
import Integrations from "./integrations";
import { CopyButton, SiteLink } from "./components";

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    to,
    hash,
    search: _search,
    activeOptions: _activeOptions,
    children,
    ...props
  }: Record<string, any>) => (
    <a href={to + (hash ? `#${hash}` : "")} {...props}>
      {children}
    </a>
  ),
}));
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});
async function run(scenario: string) {
  fireEvent.change(screen.getByLabelText("What should happen?"), {
    target: { value: scenario },
  });
  fireEvent.click(screen.getByRole("button", { name: "Run sample payment" }));
  for (let step = 0; step < 4; step++)
    await act(async () => vi.advanceTimersByTime(420));
}
describe("public product demo", () => {
  it("renders successful collection, failure and delivery retry as different outcomes", async () => {
    vi.useFakeTimers();
    render(<Home />);
    fireEvent.change(screen.getByLabelText("Payment provider"), {
      target: { value: "Stripe" },
    });
    await run("success");
    expect(screen.getByRole("status").textContent).toBe(
      "Captured, recorded, delivered.",
    );
    expect(
      screen.getByText("Stripe · illustrative routing decision"),
    ).toBeTruthy();
    // Radix tabs activate on mouse down / keyboard rather than synthetic click alone.
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Ledger" }), {
      button: 0,
      ctrlKey: false,
    });
    expect(screen.getByText("Dr ₹499.00")).toBeTruthy();
    await run("declined");
    expect(screen.getByRole("status").textContent).toContain("Declined");
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Ledger" }), {
      button: 0,
      ctrlKey: false,
    });
    expect(
      screen.getByText("No capture entries", { exact: true }),
    ).toBeTruthy();
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Webhook" }), {
      button: 0,
      ctrlKey: false,
    });
    expect(document.querySelector(".event-payload")?.textContent).toContain(
      "payment.failed",
    );
    await run("retry");
    expect(screen.getByRole("status").textContent).toContain(
      "Delivery needs a retry",
    );
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Webhook" }), {
      button: 0,
      ctrlKey: false,
    });
    fireEvent.click(
      screen.getByRole("button", { name: "Retry sample delivery" }),
    );
    expect(screen.getByRole("status").textContent).toBe(
      "Captured, recorded, delivered.",
    );
  });
  it("shows empty inspectors and cancels an in-flight sample", async () => {
    vi.useFakeTimers();
    render(<Home />);
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Ledger" }), {
      button: 0,
      ctrlKey: false,
    });
    expect(screen.getByText("No ledger entries yet")).toBeTruthy();
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Webhook" }), {
      button: 0,
      ctrlKey: false,
    });
    expect(screen.getByText("No delivery attempt yet")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Run sample payment" }));
    expect(screen.getByRole("status").textContent).toContain(
      "Following the payment",
    );
    fireEvent.click(screen.getByRole("button", { name: "Reset payment demo" }));
    await act(async () => vi.advanceTimersByTime(2000));
    expect(screen.getByRole("status").textContent).toBe("Ready when you are");
  });
  it("keeps public capability and hosting guides available", () => {
    const view = render(<Hosting />);
    expect(screen.getByRole("heading", { level: 1 })).toBeTruthy();
    view.rerender(<Integrations />);
    expect(screen.getByRole("table")).toBeTruthy();
  });
  it("handles unavailable clipboard and internal and external links", async () => {
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: vi.fn().mockRejectedValue(new Error("denied")) },
    });
    render(
      <>
        <CopyButton target="missing" />
        <SiteLink href="/docs?guide=api#scopes">Guide</SiteLink>
        <SiteLink href="https://example.com">External</SiteLink>
        <SiteLink>Empty</SiteLink>
      </>,
    );
    await act(async () =>
      fireEvent.click(screen.getByRole("button", { name: "Copy" })),
    );
    expect(
      screen.getByRole("button", { name: "Select code to copy" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "Guide" }).getAttribute("href"),
    ).toBe("/docs#scopes");
    expect(
      screen.getByRole("link", { name: "External" }).getAttribute("href"),
    ).toBe("https://example.com");
  });
});
