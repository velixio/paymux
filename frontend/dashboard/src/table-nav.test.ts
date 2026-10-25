import { describe, expect, test } from "vitest";
import { nextRow } from "./table-nav";

describe("nextRow", () => {
  test("nextRow clamps at ends", () => {
    expect(nextRow(0, 5, "up")).toBe(0);
    expect(nextRow(4, 5, "down")).toBe(4);
    expect(nextRow(2, 5, "down")).toBe(3);
    expect(nextRow(2, 5, "up")).toBe(1);
    expect(nextRow(2, 5, "first")).toBe(0);
    expect(nextRow(2, 5, "last")).toBe(4);
  });

  test("empty table returns -1", () => {
    for (const key of ["down", "up", "first", "last"] as const) {
      expect(nextRow(-1, 0, key)).toBe(-1);
      expect(nextRow(3, 0, key)).toBe(-1);
    }
  });

  test("with no row focused, down and up land on the first row", () => {
    expect(nextRow(-1, 3, "down")).toBe(0);
    expect(nextRow(-1, 3, "up")).toBe(0);
    expect(nextRow(-1, 3, "last")).toBe(2);
  });

  test("a stale index beyond the table is clamped", () => {
    expect(nextRow(9, 3, "down")).toBe(2);
    expect(nextRow(9, 3, "up")).toBe(1);
  });

  test("single row", () => {
    expect(nextRow(0, 1, "down")).toBe(0);
    expect(nextRow(0, 1, "up")).toBe(0);
  });
});
