import { expect, test } from "vitest";
import { statusLabel, statusTone } from "./pill";

// Every status the hub stores (internal/core/state, the migrations' CHECK
// lists, alerts and log levels) has a deliberate tone.
test.each([
  [
    "ok",
    [
      "succeeded",
      "paid",
      "active",
      "completed",
      "done",
      "registered",
      "captured",
      "delivered",
      "won",
      "resolved",
      "fixed",
      "ok",
      "ready",
      "sent",
      "renewal_succeeded",
      "setup_succeeded",
    ],
  ],
  [
    "bad",
    [
      "failed",
      "lost",
      "dead",
      "exhausted",
      "error",
      "critical",
      "mismatch",
      "amount_mismatch",
      "succeeded_mismatch",
      "renewal_failed",
      "disputed",
    ],
  ],
  [
    "warn",
    [
      "requires_action",
      "needs_response",
      "needs_review",
      "past_due",
      "incomplete",
      "open",
      "warning",
      "warn",
      "to_link",
      "missing",
      "problems",
    ],
  ],
  [
    "info",
    [
      "created",
      "pending",
      "processing",
      "running",
      "scheduled",
      "charging",
      "awaiting",
      "authorized",
      "trialing",
      "under_review",
      "info",
    ],
  ],
  [
    "neutral",
    [
      "canceled",
      "cancelled",
      "expired",
      "ended",
      "closed",
      "revoked",
      "refunded",
      "paused",
      "accepted",
      "incomplete_expired",
      "setup_expired",
      "debug",
      "draft",
      "",
      "unknown",
    ],
  ],
] as const)("%s tone", (tone, words) => {
  for (const w of words) expect(statusTone(w), w).toBe(tone);
});

test("case-insensitive, trims whitespace, spaces equal underscores", () => {
  expect(statusTone("  Succeeded \n")).toBe("ok");
  expect(statusTone("PAST_DUE")).toBe("warn");
  expect(statusTone("Requires Action")).toBe("warn");
});

test("labels read as words in sentence case", () => {
  expect(statusLabel("succeeded")).toBe("Succeeded");
  expect(statusLabel("requires_action")).toBe("Requires action");
  expect(statusLabel("  needs_response ")).toBe("Needs response");
  expect(statusLabel("OK")).toBe("OK");
  expect(statusLabel("live")).toBe("Live");
  expect(statusLabel("")).toBe("");
});
