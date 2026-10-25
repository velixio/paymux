import { expect, test } from "vitest";
import { SHORTCUTS, TAB_KEYS, keyLabel } from "./shortcuts";

test("key codes read as the key a person presses", () => {
  expect(keyLabel("BracketLeft")).toBe("[");
  expect(keyLabel("BracketRight")).toBe("]");
  expect(keyLabel("KeyG")).toBe("g");
  expect(keyLabel("Digit7")).toBe("7");
  expect(keyLabel("Slash")).toBe("/");
});

test("the sheet's tab shortcuts come from the bindings the shell uses", () => {
  const tabs = SHORTCUTS.find((g) => g.group === "Tabs")!.items;
  for (const binding of TAB_KEYS) {
    const item = tabs.find((i) => i.label === binding.label);
    expect(item, binding.label).toBeDefined();
    expect(item!.combos[0].keys).toEqual(
      binding.digits
        ? [...binding.keys.map(keyLabel), "1–9"]
        : binding.keys.map(keyLabel),
    );
    expect(item!.combos[0].then ?? false).toBe(
      binding.keys.length + (binding.digits ? 1 : 0) > 1,
    );
  }
});
