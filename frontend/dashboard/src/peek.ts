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
