import { expect, test } from "vitest";
import { statusTone } from "./pill";

test.each([
  ["ok", ["succeeded", "paid", "active", "completed", "done", "registered"]],
  ["bad", ["failed", "refunded", "disputed", "lost", "canceled", "cancelled"]],
  ["warn", ["pending", "processing", "past_due", "requires_action", "needs_response"]],
  ["neutral", ["draft", "", "unknown"]],
] as const)("%s tone", (tone, words) => {
  for (const w of words) expect(statusTone(w)).toBe(tone);
});

test("case-insensitive and trims whitespace", () => {
  expect(statusTone("  Succeeded \n")).toBe("ok");
  expect(statusTone("PAST_DUE")).toBe("warn");
});
