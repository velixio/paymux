import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { cleanup, screen, waitFor } from "@testing-library/react";
import { handlers, mount, resetHost } from "./host.test.helpers";

const waitingHtml =
  '<title>Waiting for external auth - Paymux</title><div id="paymux-server-page"><section data-signin-wait="/dashboard/login/waiting/status"><h3>Waiting for external auth</h3></section></div>';

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

test("the waiting page signs in by itself once the approval arrives", async () => {
  const assign = vi.fn();
  vi.stubGlobal("location", {
    origin: location.origin,
    href: location.href,
    pathname: location.pathname,
    search: location.search,
    hash: location.hash,
    assign,
  });
  handlers["/dashboard/login/waiting"] = () => waitingHtml;
  handlers["/dashboard/login/waiting/status"] = () => "ready";

  await mount("/dashboard/login/waiting");
  expect(await screen.findByText("Waiting for external auth")).toBeTruthy();

  await waitFor(() => expect(assign).toHaveBeenCalledTimes(1), {
    timeout: 4000,
  });
  expect(assign).toHaveBeenCalledWith("/dashboard/login/waiting");
});
