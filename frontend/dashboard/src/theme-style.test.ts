import { setNonce } from "get-nonce";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { applyDashboardTheme, clearThemeCache } from "./theme-apply";

beforeEach(() => {
  document.head.innerHTML = "";
  clearThemeCache();
  setNonce("n0nce");
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({
      ok: true,
      status: 200,
      text: async () =>
        JSON.stringify({
          name: "stripe",
          scheme: "",
          seeds: { accent: "#635bff" },
          dark_seeds: null,
          overrides: {},
        }),
    })),
  );
});
afterEach(() => vi.unstubAllGlobals());

// Chrome checks an inline <style> against style-src when it is inserted, so it
// must already carry its nonce and its text then (a nonce-less empty element is
// reported as a Content Security Policy violation).
test("the theme style is complete when it is appended", async () => {
  const seen: { nonce: string | null; text: string }[] = [];
  const append = document.head.append.bind(document.head);
  vi.spyOn(document.head, "append").mockImplementation((...nodes) => {
    for (const n of nodes)
      if (n instanceof HTMLStyleElement)
        seen.push({
          nonce: n.getAttribute("nonce"),
          text: n.textContent ?? "",
        });
    append(...nodes);
  });
  await applyDashboardTheme("stripe");
  expect(seen).toHaveLength(1);
  expect(seen[0].nonce).toBe("n0nce");
  expect(seen[0].text).toContain("--primary:#635bff");
});

test("an existing style without the nonce is replaced, not edited", async () => {
  const stale = document.createElement("style");
  stale.id = "paymux-dashboard-theme";
  stale.textContent = "x{}";
  document.head.append(stale);
  await applyDashboardTheme("stripe");
  const tags = document.querySelectorAll("style#paymux-dashboard-theme");
  expect(tags).toHaveLength(1);
  expect(tags[0]).not.toBe(stale);
  expect(tags[0].getAttribute("nonce")).toBe("n0nce");
});
