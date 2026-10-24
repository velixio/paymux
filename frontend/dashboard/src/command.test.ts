import { describe, expect, test } from "vitest";
import { intents, rank, recent, remember, type Item } from "./command";

function memory(): Pick<Storage, "getItem" | "setItem"> & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return {
    data,
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
  };
}

const throwing: Pick<Storage, "getItem" | "setItem"> = {
  getItem() {
    throw new Error("denied");
  },
  setItem() {
    throw new Error("denied");
  },
};

const it = (id: string, label: string, hint?: string): Item => ({ id, label, hint });

describe("intents", () => {
  test("pay_ id goes to /dashboard/payments/<id>", () => {
    const [first] = intents("pay_01abc");
    expect(first.href).toBe("/dashboard/payments/pay_01abc");
  });

  test("sub_ id goes to /dashboard/subscriptions/<id>", () => {
    expect(intents("sub_9z")[0].href).toBe("/dashboard/subscriptions/sub_9z");
  });

  test("other record ids go to their pages", () => {
    expect(intents("inv_1")[0].href).toBe("/dashboard/invoices/inv_1");
    expect(intents("crn_1")[0].href).toBe("/dashboard/credit-notes/crn_1");
    expect(intents("dsp_1")[0].href).toBe("/dashboard/disputes/dsp_1");
  });

  test("email searches payments and subscriptions", () => {
    expect(intents("asha@example.com").map((i) => i.href)).toEqual([
      "/dashboard/payments?q=asha%40example.com",
      "/dashboard/subscriptions?q=asha%40example.com",
    ]);
  });

  test("free text searches payments and invoices", () => {
    expect(intents("order 42").map((i) => i.href)).toEqual([
      "/dashboard/payments?q=order%2042",
      "/dashboard/invoices?q=order%2042",
    ]);
  });

  test("blank query has no intents; input is trimmed", () => {
    expect(intents("   ")).toEqual([]);
    expect(intents("  pay_1 ")[0].href).toBe("/dashboard/payments/pay_1");
  });

  test("an id with path characters is not treated as an id", () => {
    expect(intents("pay_1/../x")[0].href).toBe("/dashboard/payments?q=pay_1%2F..%2Fx");
  });

  test("intent ids are unique", () => {
    const ids = intents("a@b.co").map((i) => i.id);
    expect(new Set(ids).size).toBe(ids.length);
  });
});

describe("rank", () => {
  const items = [
    it("1", "Open payments", "go to payments list"),
    it("2", "Payment links"),
    it("3", "Create payment link", "links"),
    it("4", "Toggle theme", "dark mode pay"),
  ];

  test("rank prefers label prefix", () => {
    const out = rank(items, "pay").map((i) => i.id);
    expect(out[0]).toBe("2");
    expect(out.indexOf("1")).toBeLessThan(out.indexOf("4")); // label substring beats hint-only
    expect(out.at(-1)).toBe("4");
  });

  test("subsequence matches, case-insensitively", () => {
    expect(rank(items, "TGL").map((i) => i.id)).toEqual(["4"]);
    expect(rank(items, "zzz")).toEqual([]);
  });

  test("empty query keeps the order", () => {
    expect(rank(items, "")).toEqual(items);
  });

  test("ties keep the original order", () => {
    const two = [it("a", "alpha"), it("b", "alpha")];
    expect(rank(two, "alp").map((i) => i.id)).toEqual(["a", "b"]);
  });

  test("hint can match when the label does not", () => {
    expect(rank([it("x", "Zebra", "billing")], "bill").map((i) => i.id)).toEqual(["x"]);
  });
});

describe("recent and remember", () => {
  test("recent caps at 8 and dedupes", () => {
    const s = memory();
    for (let n = 0; n < 12; n++) remember(s, "u1", it(`i${n}`, `Item ${n}`, "/p"));
    remember(s, "u1", it("i5", "Item 5 again"));
    const got = recent(s, "u1");
    expect(got).toHaveLength(8);
    expect(got[0]).toMatchObject({ id: "i5", label: "Item 5 again" });
    expect(got.filter((i) => i.id === "i5")).toHaveLength(1);
    expect(got.map((i) => i.id)).toEqual(["i5", "i11", "i10", "i9", "i8", "i7", "i6", "i4"]);
  });

  test("recent is per user", () => {
    const s = memory();
    remember(s, "a", it("x", "X"));
    expect(recent(s, "b")).toEqual([]);
    expect(s.data.has("paymux-recent:a")).toBe(true);
  });

  test("recent survives throwing storage", () => {
    expect(recent(throwing, "u")).toEqual([]);
    expect(() => remember(throwing, "u", it("x", "X"))).not.toThrow();
  });

  test("null storage is fine", () => {
    expect(recent(null, "u")).toEqual([]);
    expect(() => remember(null, "u", it("x", "X"))).not.toThrow();
  });

  test("corrupt stored data is ignored", () => {
    const s = memory();
    s.data.set("paymux-recent:u", "{nope");
    expect(recent(s, "u")).toEqual([]);
    s.data.set("paymux-recent:u", JSON.stringify([{ id: 1 }, { id: "ok", label: "Ok" }, "str", null]));
    expect(recent(s, "u")).toEqual([{ id: "ok", label: "Ok" }]);
    s.data.set("paymux-recent:u", JSON.stringify({ not: "an array" }));
    expect(recent(s, "u")).toEqual([]);
  });
});
