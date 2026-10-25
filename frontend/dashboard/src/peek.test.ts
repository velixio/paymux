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
