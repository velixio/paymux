import { describe, expect, test, vi } from "vitest";
import { createMatcher, isTyping, type Binding } from "./keys";

type Init = Partial<
  Pick<KeyboardEvent, "code" | "key" | "ctrlKey" | "altKey" | "metaKey" | "shiftKey">
> & { target?: EventTarget | null };

const ev = (code: string, init: Init = {}) =>
  ({
    code,
    key: code,
    ctrlKey: false,
    altKey: false,
    metaKey: false,
    shiftKey: false,
    target: null,
    ...init,
  }) as unknown as KeyboardEvent;

function setup(extra: Binding[] = []) {
  const runs = { one: vi.fn(), seq: vi.fn(), br: vi.fn(), shifted: vi.fn() };
  const bindings: Binding[] = [
    { keys: ["KeyG", "Digit1"], run: runs.seq },
    { keys: ["KeyJ"], run: runs.one },
    { keys: ["BracketLeft"], run: runs.br },
    { keys: ["Slash"], shift: true, run: runs.shifted },
    ...extra,
  ];
  return { runs, match: createMatcher(bindings) };
}

describe("matcher", () => {
  test("single key fires", () => {
    const { runs, match } = setup();
    expect(match(ev("KeyJ"), 0)).toBe(true);
    expect(runs.one).toHaveBeenCalledTimes(1);
  });

  test("unbound keys are not handled", () => {
    const { match } = setup();
    expect(match(ev("KeyZ"), 0)).toBe(false);
  });

  test("g then 1 fires within timeout", () => {
    const { runs, match } = setup();
    expect(match(ev("KeyG"), 0)).toBe(true);
    expect(runs.seq).not.toHaveBeenCalled();
    expect(match(ev("Digit1"), 800)).toBe(true);
    expect(runs.seq).toHaveBeenCalledTimes(1);
  });

  test("g then 1 after timeout does not fire", () => {
    const { runs, match } = setup();
    match(ev("KeyG"), 0);
    expect(match(ev("Digit1"), 801)).toBe(false);
    expect(runs.seq).not.toHaveBeenCalled();
  });

  test("a prefix that matches nothing resets, and the key may start a new sequence", () => {
    const { runs, match } = setup();
    match(ev("KeyG"), 0);
    expect(match(ev("KeyJ"), 10)).toBe(true);
    expect(runs.one).toHaveBeenCalledTimes(1);
    match(ev("KeyG"), 20);
    match(ev("KeyG"), 30);
    match(ev("Digit1"), 40);
    expect(runs.seq).toHaveBeenCalledTimes(1);
    expect(match(ev("Digit1"), 50)).toBe(false);
  });

  test("a completed sequence resets the buffer", () => {
    const { runs, match } = setup();
    match(ev("KeyG"), 0);
    match(ev("Digit1"), 1);
    expect(match(ev("Digit1"), 2)).toBe(false);
    expect(runs.seq).toHaveBeenCalledTimes(1);
  });

  test("custom timeout", () => {
    const run = vi.fn();
    const match = createMatcher([{ keys: ["KeyA", "KeyB"], run }], 100);
    match(ev("KeyA"), 0);
    expect(match(ev("KeyB"), 101)).toBe(false);
    match(ev("KeyA"), 200);
    expect(match(ev("KeyB"), 300)).toBe(true);
  });

  test("matches by code not key", () => {
    const { runs, match } = setup();
    expect(match(ev("BracketLeft", { key: "ü" }), 0)).toBe(true);
    expect(runs.br).toHaveBeenCalledTimes(1);
  });

  test("ignores events with ctrl/alt/meta", () => {
    const { runs, match } = setup();
    for (const mod of ["ctrlKey", "altKey", "metaKey"] as const)
      expect(match(ev("KeyJ", { [mod]: true }), 0)).toBe(false);
    expect(runs.one).not.toHaveBeenCalled();
  });

  test("shift is allowed; a shift binding needs it", () => {
    const { runs, match } = setup();
    expect(match(ev("Slash"), 0)).toBe(false);
    expect(match(ev("Slash", { shiftKey: true }), 1)).toBe(true);
    expect(runs.shifted).toHaveBeenCalledTimes(1);
    expect(match(ev("KeyJ", { shiftKey: true }), 2)).toBe(true);
  });

  test("ignored while typing in input/textarea/select/contenteditable", () => {
    const { runs, match } = setup();
    for (const tag of ["input", "textarea", "select"]) {
      const el = document.createElement(tag);
      expect(match(ev("KeyJ", { target: el }), 0)).toBe(false);
    }
    const div = document.createElement("div");
    div.setAttribute("contenteditable", "true");
    expect(match(ev("KeyJ", { target: div }), 0)).toBe(false);
    expect(runs.one).not.toHaveBeenCalled();
  });
});

describe("isTyping", () => {
  test("recognises editable targets only", () => {
    expect(isTyping(null)).toBe(false);
    expect(isTyping(window)).toBe(false);
    expect(isTyping(document.createElement("button"))).toBe(false);
    expect(isTyping(document.createElement("input"))).toBe(true);
    const ce = document.createElement("div");
    ce.setAttribute("contenteditable", "");
    expect(isTyping(ce)).toBe(true);
    ce.setAttribute("contenteditable", "false");
    expect(isTyping(ce)).toBe(false);
  });
});
