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
  test("peekOf reads a record path and rejects everything else", () => {
    const enc = encodeURIComponent("/dashboard/payments/pay_1");
    expect(peekOf(`?peek=${enc}`)).toBe("/dashboard/payments/pay_1");
    expect(peekOf(`?status=failed&peek=${enc}`)).toBe(
      "/dashboard/payments/pay_1",
    );
    for (const bad of [
      "",
      "?peek=",
      "?peek=/dashboard/payments",
      "?peek=https://evil.example/dashboard/payments/pay_1",
      "?peek=//evil.example/dashboard/payments/pay_1",
      "?peek=/dashboard/payments/pay_1/refund",
      "?peek=javascript:alert(1)",
      "?other=1",
    ])
      expect(peekOf(bad), bad).toBeNull();
  });

  test("withPeek adds, replaces and removes the parameter, keeping the rest", () => {
    expect(withPeek("/dashboard/payments", "/dashboard/payments/pay_1")).toBe(
      "/dashboard/payments?peek=%2Fdashboard%2Fpayments%2Fpay_1",
    );
    expect(
      withPeek(
        "/dashboard/payments?status=failed&peek=%2Fdashboard%2Fpayments%2Fpay_1",
        "/dashboard/payments/pay_2",
      ),
    ).toBe(
      "/dashboard/payments?status=failed&peek=%2Fdashboard%2Fpayments%2Fpay_2",
    );
    expect(
      withPeek(
        "/dashboard/payments?status=failed&peek=%2Fdashboard%2Fpayments%2Fpay_1",
        null,
      ),
    ).toBe("/dashboard/payments?status=failed");
    expect(withPeek("/dashboard/payments?peek=%2Fx", null)).toBe(
      "/dashboard/payments",
    );
  });

  test("peekClickTarget: a record link on a list page becomes list + peek", () => {
    expect(
      peekClickTarget(
        "/dashboard/payments/pay_1",
        "/dashboard/payments?status=failed",
      ),
    ).toBe(
      "/dashboard/payments?status=failed&peek=%2Fdashboard%2Fpayments%2Fpay_1",
    );
    // from a record page, a list link, a download or an external link: no peek
    expect(
      peekClickTarget("/dashboard/apps/app_1", "/dashboard/payments/pay_1"),
    ).toBeNull();
    expect(
      peekClickTarget("/dashboard/payments", "/dashboard/payments"),
    ).toBeNull();
    expect(
      peekClickTarget("/dashboard/invoices/inv_1.pdf", "/dashboard/invoices"),
    ).toBeNull();
    expect(
      peekClickTarget("/dashboard/payments/pay_1", "/dashboard/login"),
    ).toBeNull();
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
