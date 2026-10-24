// Keys are KeyboardEvent.code values, so bindings work on any layout.
// `shift: true` requires Shift on the final key (for `?`); otherwise Shift is
// ignored. Ctrl, Alt and Meta never match.
export type Binding = { keys: string[]; shift?: boolean; run: () => void };

export function isTyping(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  if (["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName)) return true;
  const ce = target.getAttribute("contenteditable");
  return target.isContentEditable === true || (ce !== null && ce !== "false");
}

export function createMatcher(
  bindings: Binding[],
  timeoutMs = 800,
): (e: KeyboardEvent, now: number) => boolean {
  let buffer: string[] = [];
  let last = 0;

  const startsWith = (seq: string[]) => (b: Binding) =>
    b.keys.length > seq.length && seq.every((k, i) => b.keys[i] === k);

  return (e, now) => {
    if (e.ctrlKey || e.altKey || e.metaKey || isTyping(e.target)) return false;
    const fresh = now - last > timeoutMs ? [] : buffer;
    last = now;
    buffer = [];
    // A key that breaks the pending sequence may still start a new one.
    const attempts = fresh.length ? [[...fresh, e.code], [e.code]] : [[e.code]];
    for (const seq of attempts) {
      const hit = bindings.find(
        (b) =>
          b.keys.length === seq.length &&
          seq.every((k, i) => b.keys[i] === k) &&
          (!b.shift || e.shiftKey),
      );
      if (hit) {
        hit.run();
        return true;
      }
      if (bindings.some(startsWith(seq))) {
        buffer = seq;
        return true;
      }
    }
    return false;
  };
}
