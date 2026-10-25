import { describe, expect, test } from "vitest";
import { isRecordPath } from "./peek";

describe("isRecordPath", () => {
  test("record pages are /dashboard/<section>/<id>", () => {
    for (const p of [
      "/dashboard/payments/pay_1",
      "/dashboard/subscriptions/sub_1",
      "/dashboard/invoices/inv_1",
      "/dashboard/credit-notes/crn_1",
      "/dashboard/disputes/dsp_1",
      "/dashboard/apps/app_1",
      "/dashboard/providers/pa_1",
      "/dashboard/tax-entities/te_1",
      "/dashboard/payments/pay_1?x=1#tab-events",
    ])
      expect(isRecordPath(p), p).toBe(true);
  });

  test("lists, deeper paths, downloads and auth pages are not records", () => {
    for (const p of [
      "/dashboard/payments",
      "/dashboard/payments/",
      "/dashboard/overview",
      "/dashboard/payments/pay_1/refund",
      "/dashboard/overview/revenue.csv",
      "/dashboard/invoices/inv_1.pdf",
      "/dashboard/login/totp",
      "/dashboard/account/sessions",
      "/dashboard/payments/new",
      "/other/payments/pay_1",
      "/dashboard",
      "",
    ])
      expect(isRecordPath(p), p).toBe(false);
  });
});

import {
  clampWidth,
  loadWidth,
  peekClickTarget,
  peekOf,
  PEEK_DEFAULT,
  PEEK_MAX,
  PEEK_MIN,
  saveWidth,
  withPeek,
} from "./peek";

describe("peek URL parameter", () => {
  const L = "/dashboard/payments";
  test("peekOf reads the short id of the list's own record", () => {
    expect(peekOf(`${L}?peek=pay_1`)).toBe(`${L}/pay_1`);
    expect(peekOf(`${L}?status=failed&peek=pay_1`)).toBe(`${L}/pay_1`);
  });

  test("peekOf still reads the long form of older links", () => {
    const enc = encodeURIComponent("/dashboard/subscriptions/sub_1");
    expect(peekOf(`${L}?peek=${enc}`)).toBe("/dashboard/subscriptions/sub_1");
  });

  test("peekOf rejects everything that is not a record", () => {
    for (const bad of [
      L,
      `${L}?peek=`,
      `${L}?peek=pay_1/refund`,
      `${L}?peek=..`,
      `${L}?peek=pay_1.pdf`,
      `${L}?peek=javascript:alert(1)`,
      `${L}?peek=/dashboard/payments`,
      `${L}?peek=https://evil.example/dashboard/payments/pay_1`,
      `${L}?peek=//evil.example/dashboard/payments/pay_1`,
      `${L}?peek=/dashboard/payments/pay_1/refund`,
      `${L}/pay_9?peek=pay_1`, // a short id only means something on a list
      `${L}?other=1`,
    ])
      expect(peekOf(bad), bad).toBeNull();
  });

  test("withPeek writes the short id for the list's own records", () => {
    expect(withPeek(L, `${L}/pay_1`)).toBe(`${L}?peek=pay_1`);
    expect(withPeek(`${L}?status=failed&peek=pay_1`, `${L}/pay_2`)).toBe(
      `${L}?status=failed&peek=pay_2`,
    );
  });

  test("withPeek keeps the full path for a record of another section", () => {
    expect(withPeek(L, "/dashboard/subscriptions/sub_1")).toBe(
      `${L}?peek=%2Fdashboard%2Fsubscriptions%2Fsub_1`,
    );
  });

  test("withPeek removes the parameter, keeping the rest", () => {
    expect(withPeek(`${L}?status=failed&peek=pay_1`, null)).toBe(
      `${L}?status=failed`,
    );
    expect(withPeek(`${L}?peek=pay_1`, null)).toBe(L);
  });

  test("peekClickTarget: a record link on a list page becomes list + peek", () => {
    expect(peekClickTarget(`${L}/pay_1`, `${L}?status=failed`)).toBe(
      `${L}?status=failed&peek=pay_1`,
    );
    // from a record page, a list link, a download or an external link: no peek
    expect(
      peekClickTarget("/dashboard/apps/app_1", "/dashboard/payments/pay_1"),
    ).toBeNull();
    expect(peekClickTarget(L, L)).toBeNull();
    expect(
      peekClickTarget("/dashboard/invoices/inv_1.pdf", "/dashboard/invoices"),
    ).toBeNull();
    expect(peekClickTarget(`${L}/pay_1`, "/dashboard/login")).toBeNull();
  });
});

describe("pane width", () => {
  test("clampWidth keeps 360..640 and falls back to the default", () => {
    expect([PEEK_MIN, PEEK_DEFAULT, PEEK_MAX]).toEqual([360, 420, 640]);
    expect(clampWidth(100)).toBe(360);
    expect(clampWidth(500)).toBe(500);
    expect(clampWidth(9000)).toBe(640);
    expect(clampWidth(Number.NaN)).toBe(420);
  });

  test("width is stored per browser and survives broken storage", () => {
    const data = new Map<string, string>();
    const s = {
      getItem: (k: string) => data.get(k) ?? null,
      setItem: (k: string, v: string) => void data.set(k, v),
    };
    expect(loadWidth(s)).toBe(420);
    saveWidth(s, 555);
    expect(data.get("paymux-peek-width")).toBe("555");
    expect(loadWidth(s)).toBe(555);
    data.set("paymux-peek-width", "9999");
    expect(loadWidth(s)).toBe(640);
    data.set("paymux-peek-width", "abc");
    expect(loadWidth(s)).toBe(420);
    const broken = {
      getItem(): string | null {
        throw new Error("denied");
      },
      setItem(): void {
        throw new Error("denied");
      },
    };
    expect(loadWidth(broken)).toBe(420);
    expect(() => saveWidth(broken, 500)).not.toThrow();
    expect(loadWidth(null)).toBe(420);
    expect(() => saveWidth(null, 500)).not.toThrow();
  });
});
