import { describe, expect, test } from "vitest";
import { add, filterQuery, list, remove, sameQuery } from "./saved-filters";

function memory() {
  const data = new Map<string, string>();
  return {
    data,
    getItem: (k: string) => data.get(k) ?? null,
    setItem: (k: string, v: string) => void data.set(k, v),
  };
}

const throwing = {
  getItem(): string | null {
    throw new Error("denied");
  },
  setItem(): void {
    throw new Error("denied");
  },
};

const P = "/dashboard/payments";

describe("saved filters", () => {
  test("add then list", () => {
    const s = memory();
    add(s, "u1", P, { name: "Failed", query: "status=failed" });
    add(s, "u1", P, { name: "Big", query: "q=1000" });
    expect(list(s, "u1", P)).toEqual([
      { name: "Failed", query: "status=failed" },
      { name: "Big", query: "q=1000" },
    ]);
    expect(s.data.has("paymux-filters:u1:/dashboard/payments")).toBe(true);
  });

  test("lists are per user and per path", () => {
    const s = memory();
    add(s, "u1", P, { name: "A", query: "a=1" });
    expect(list(s, "u2", P)).toEqual([]);
    expect(list(s, "u1", "/dashboard/invoices")).toEqual([]);
  });

  test("same name replaces", () => {
    const s = memory();
    add(s, "u", P, { name: "A", query: "a=1" });
    add(s, "u", P, { name: "B", query: "b=1" });
    add(s, "u", P, { name: "A", query: "a=2" });
    expect(list(s, "u", P)).toEqual([
      { name: "A", query: "a=2" },
      { name: "B", query: "b=1" },
    ]);
  });

  test("cap 12 drops the oldest", () => {
    const s = memory();
    for (let n = 0; n < 15; n++)
      add(s, "u", P, { name: `f${n}`, query: `n=${n}` });
    const got = list(s, "u", P);
    expect(got).toHaveLength(12);
    expect(got[0].name).toBe("f3");
    expect(got.at(-1)?.name).toBe("f14");
  });

  test("remove deletes by name and ignores unknown names", () => {
    const s = memory();
    add(s, "u", P, { name: "A", query: "a=1" });
    add(s, "u", P, { name: "B", query: "b=1" });
    remove(s, "u", P, "A");
    remove(s, "u", P, "nope");
    expect(list(s, "u", P)).toEqual([{ name: "B", query: "b=1" }]);
  });

  test("blank names are not saved", () => {
    const s = memory();
    add(s, "u", P, { name: "   ", query: "a=1" });
    expect(list(s, "u", P)).toEqual([]);
  });

  test("throwing storage returns []", () => {
    expect(list(throwing, "u", P)).toEqual([]);
    expect(() => add(throwing, "u", P, { name: "A", query: "" })).not.toThrow();
    expect(() => remove(throwing, "u", P, "A")).not.toThrow();
  });

  test("null storage is fine", () => {
    expect(list(null, "u", P)).toEqual([]);
    expect(() => add(null, "u", P, { name: "A", query: "" })).not.toThrow();
    expect(() => remove(null, "u", P, "A")).not.toThrow();
  });

  test("corrupt stored data is ignored", () => {
    const s = memory();
    const k = "paymux-filters:u:" + P;
    s.data.set(k, "{nope");
    expect(list(s, "u", P)).toEqual([]);
    s.data.set(k, JSON.stringify({ not: "array" }));
    expect(list(s, "u", P)).toEqual([]);
    s.data.set(
      k,
      JSON.stringify([
        { name: 1, query: "x" },
        { name: "ok", query: "a=1" },
        null,
        { name: "noq" },
      ]),
    );
    expect(list(s, "u", P)).toEqual([{ name: "ok", query: "a=1" }]);
  });
});

describe("filterQuery", () => {
  test("query drops csrf and empties", () => {
    const fd = new FormData();
    fd.append("csrf", "tok");
    fd.append("status", "failed");
    fd.append("q", "");
    fd.append("from", "2027-01-01");
    fd.append("app", "");
    expect(filterQuery(fd.entries())).toBe("status=failed&from=2027-01-01");
  });

  test("encodes values, keeps repeated keys, skips files", () => {
    const entries: [string, FormDataEntryValue][] = [
      ["q", "a b&c"],
      ["flag", "x"],
      ["flag", "y"],
      ["upload", new File(["x"], "x.txt")],
    ];
    expect(filterQuery(entries)).toBe("q=a+b%26c&flag=x&flag=y");
  });

  test("nothing set gives an empty string", () => {
    expect(filterQuery([])).toBe("");
  });
});

describe("sameQuery", () => {
  test("order, empty values, csrf and peek do not matter", () => {
    expect(
      sameQuery(
        "status=failed&app=a1",
        "app=a1&q=&csrf=x&status=failed&peek=%2Fdashboard%2Fpayments%2Fpay_1",
      ),
    ).toBe(true);
  });
  test("a different value or an extra filter is not the same", () => {
    expect(sameQuery("status=failed", "status=succeeded")).toBe(false);
    expect(sameQuery("status=failed", "status=failed&app=a1")).toBe(false);
  });
  test("an empty saved query never matches", () => {
    expect(sameQuery("", "")).toBe(false);
  });
});
