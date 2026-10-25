import { expect, test } from "vitest";
import { agoText } from "./status";

test("agoText reads like a status bar", () => {
  expect(agoText(0)).toBe("just now");
  expect(agoText(4_999)).toBe("just now");
  expect(agoText(12_000)).toBe("12 s ago");
  expect(agoText(59_999)).toBe("59 s ago");
  expect(agoText(60_000)).toBe("1 min ago");
  expect(agoText(15 * 60_000)).toBe("15 min ago");
  expect(agoText(3_600_000)).toBe("1 h ago");
  expect(agoText(5 * 3_600_000 + 1)).toBe("5 h ago");
  expect(agoText(-3_000)).toBe("just now");
});
