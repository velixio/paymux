// Record pages: /dashboard/<section>/<id>, e.g. /dashboard/payments/pay_1.
// Shared by the inner section tabs and the side pane.
const NOT_RECORDS = new Set([
  "login",
  "logout",
  "reauth",
  "account",
  "overview",
  "setup",
]);

export function isRecordPath(href: string): boolean {
  const path = href.split(/[?#]/)[0];
  const parts = path.split("/");
  // ["", "dashboard", section, id]
  if (parts.length !== 4 || parts[0] !== "" || parts[1] !== "dashboard")
    return false;
  const [, , section, id] = parts;
  if (!section || !id || id === "new" || id.includes(".")) return false;
  return !NOT_RECORDS.has(section);
}

// ---- side pane (?peek=<record path>) ----

export const PEEK_MIN = 360;
export const PEEK_DEFAULT = 420;
export const PEEK_MAX = 640;
const WIDTH_KEY = "paymux-peek-width";

export function clampWidth(n: number): number {
  return Number.isFinite(n)
    ? Math.min(PEEK_MAX, Math.max(PEEK_MIN, Math.round(n)))
    : PEEK_DEFAULT;
}

export function loadWidth(storage: Pick<Storage, "getItem"> | null): number {
  try {
    const raw = storage?.getItem(WIDTH_KEY);
    return raw ? clampWidth(Number(raw)) : PEEK_DEFAULT;
  } catch {
    return PEEK_DEFAULT;
  }
}

export function saveWidth(
  storage: Pick<Storage, "setItem"> | null,
  width: number,
): void {
  try {
    storage?.setItem(WIDTH_KEY, String(clampWidth(width)));
  } catch {
    /* the width is a convenience */
  }
}

/** The record the pane shows: a valid /dashboard record path in ?peek=. */
export function peekOf(search: string): string | null {
  const value = new URLSearchParams(search).get("peek");
  return value && value.startsWith("/dashboard/") && isRecordPath(value)
    ? value
    : null;
}

/** href with ?peek= set to a record path, or removed when record is null. */
export function withPeek(href: string, record: string | null): string {
  const url = new URL(href, "http://peek.invalid");
  if (record) url.searchParams.set("peek", record);
  else url.searchParams.delete("peek");
  return url.pathname + url.search;
}

/**
 * Where a plain click on a link of a list page goes: the same list with the
 * record in the pane. Null when the link is not a record or the current page
 * is not a list (record pages and sign-in navigate as usual).
 */
export function peekClickTarget(link: string, current: string): string | null {
  const to = new URL(link, "http://peek.invalid");
  const here = new URL(current, "http://peek.invalid");
  const record = to.pathname + to.search;
  if (!isRecordPath(to.pathname) || !to.pathname.startsWith("/dashboard/"))
    return null;
  if (
    !here.pathname.startsWith("/dashboard/") ||
    isRecordPath(here.pathname) ||
    NOT_RECORDS.has(here.pathname.split("/")[2] ?? "")
  )
    return null;
  return withPeek(here.pathname + here.search, record);
}

/** The side pane needs room: desktop widths only. */
export const isWide = (): boolean =>
  typeof matchMedia === "function" && matchMedia("(min-width: 1100px)").matches;

// The open pane registers a guard so that anything switching it to another
// record (a row click, j/k) can ask about unsaved edits first.
let guard: ((run: () => void) => void) | null = null;
export const setPeekGuard = (g: typeof guard) => {
  guard = g;
};
/** Runs a switch of the pane's record, after the pane's own confirmation. */
export const guardPeek = (run: () => void) => (guard ? guard(run) : run());
