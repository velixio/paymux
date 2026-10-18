import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import Documentation from "./docs";

const router = vi.hoisted(() => ({ pathname: "/docs" }));
vi.mock("@tanstack/react-router", () => ({
  useLocation: ({
    select,
  }: {
    select: (location: { pathname: string }) => unknown;
  }) => select({ pathname: router.pathname }),
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
  router.pathname = "/docs";
});

describe("documentation experience", () => {
  it("renders repository content, named navigation, tables, and code", () => {
    router.pathname = "/docs/api";
    render(<Documentation />);
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe(
      "Application integration",
    );
    expect(
      screen.getByRole("navigation", { name: "Documentation guides" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("navigation", { name: "On this page" }),
    ).toBeTruthy();
    expect(
      screen.getAllByRole("region", { name: "Scrollable reference table" })
        .length,
    ).toBeGreaterThan(0);
    expect(
      screen.getAllByRole("button", { name: "Copy" }).length,
    ).toBeGreaterThan(0);
    expect(
      screen.getByRole("link", { name: "Webhooks" }).getAttribute("href"),
    ).toBe("/docs/webhooks");
    expect(document.title).toBe(
      "Application integration · Paymux documentation",
    );
  });
  it("searches body content and shows a usable empty state", () => {
    render(<Documentation />);
    const search = screen.getByRole("searchbox", {
      name: "Search documentation",
    });
    fireEvent.change(search, { target: { value: "HMAC-SHA256" } });
    expect(
      screen.getByRole("heading", { name: "Search the documentation" }),
    ).toBeTruthy();
    expect(screen.getByRole("status").textContent).toContain("matching");
    const result = screen.getByRole("link", {
      name: /Webhooks Verify signatures/,
    });
    fireEvent.click(result);
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe(
      "Paymux documentation",
    );
    fireEvent.change(search, { target: { value: "not-a-real-topic" } });
    expect(screen.getByRole("status").textContent).toContain("0 guides");
    fireEvent.click(
      screen.getByRole("button", { name: "Clear documentation search" }),
    );
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe(
      "Paymux documentation",
    );
  });
  it("supports mobile disclosure, adjacent guides and an unknown slug", () => {
    const view = render(<Documentation />);
    const toggle = screen.getByRole("button", { name: "Browse all guides" });
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(
      screen.getByRole("navigation", { name: "Adjacent guides" }).textContent,
    ).toContain("Installation");
    router.pathname = "/docs/missing";
    view.rerender(<Documentation />);
    expect(
      screen.getByRole("heading", { name: "Guide not found" }),
    ).toBeTruthy();
  });
  it("copies the exact visible example text", async () => {
    vi.useFakeTimers();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
    router.pathname = "/docs/api";
    render(<Documentation />);
    await act(async () =>
      fireEvent.click(screen.getAllByRole("button", { name: "Copy" })[0]),
    );
    expect(writeText.mock.calls[0][0]).toContain("Authorization: Bearer");
    expect(screen.getByRole("button", { name: "Copied" })).toBeTruthy();
    await act(async () => vi.advanceTimersByTime(1800));
    expect(screen.queryByRole("button", { name: "Copied" })).toBeNull();
    vi.useRealTimers();
  });
});

it("renders long guides and scrolls a hash anchor after navigation", async () => {
  router.pathname = "/docs/runbook";
  history.replaceState(null, "", "/#layout");
  const scroll = vi.fn();
  const original = HTMLElement.prototype.scrollIntoView;
  HTMLElement.prototype.scrollIntoView = scroll;
  const raf = vi
    .spyOn(globalThis, "requestAnimationFrame")
    .mockImplementation((callback) => {
      callback(0);
      return 0;
    });
  render(<Documentation />);
  expect(scroll).toHaveBeenCalledOnce();
  expect(screen.getAllByRole("heading", { level: 3 }).length).toBeGreaterThan(
    0,
  );
  raf.mockRestore();
  HTMLElement.prototype.scrollIntoView = original;
  history.replaceState(null, "", "/");
});

it("ends the reading sequence without inventing another guide", () => {
  router.pathname = "/docs/testing";
  render(<Documentation />);
  const pagination = screen.getByRole("navigation", {
    name: "Adjacent guides",
  });
  expect(pagination.textContent).toContain("Previous");
  expect(pagination.textContent).not.toContain("Next");
});
