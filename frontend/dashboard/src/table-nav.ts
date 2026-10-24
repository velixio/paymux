// Row to focus after a navigation key in a data table. -1 means no row
// (empty table); with no row focused (current < 0) down and up land on the
// first row. Never wraps.
export function nextRow(current: number, count: number, key: "down" | "up" | "first" | "last"): number {
  if (count <= 0) return -1;
  const last = count - 1;
  const at = Math.min(current, last); // a stale index is clamped into the table
  switch (key) {
    case "first":
      return 0;
    case "last":
      return last;
    case "down":
      return at < 0 ? 0 : Math.min(at + 1, last);
    case "up":
      return at < 0 ? 0 : Math.max(at - 1, 0);
  }
}
