// Status tones, the convention payment dashboards use: green done well, red
// failed, amber needs you, blue in progress, grey finished with nothing to do.
export type Tone = "ok" | "bad" | "warn" | "info" | "neutral";

const TONES: Record<string, Tone> = {};
const tone = (t: Tone, words: string[]) => {
  for (const w of words) TONES[w] = t;
};
tone("ok", [
  "succeeded",
  "paid",
  "active",
  "completed",
  "done",
  "registered",
  "captured",
  "delivered",
  "won",
  "resolved",
  "fixed",
  "ok",
  "ready",
  "sent",
  "renewal_succeeded",
  "setup_succeeded",
  "setup_succeeded_trial",
  "activated",
]);
tone("bad", [
  "failed",
  "lost",
  "dead",
  "exhausted",
  "error",
  "critical",
  "mismatch",
  "amount_mismatch",
  "succeeded_mismatch",
  "renewal_failed",
  "disputed",
]);
tone("warn", [
  "requires_action",
  "needs_response",
  "needs_review",
  "past_due",
  "incomplete",
  "open",
  "warning",
  "warn",
  "to_link",
  "missing",
  "problems",
]);
tone("info", [
  "created",
  "pending",
  "processing",
  "running",
  "scheduled",
  "charging",
  "awaiting",
  "authorized",
  "trialing",
  "under_review",
  "info",
  "this_session",
]);
// Everything else, e.g. canceled, expired, ended, refunded, is neutral.

const key = (text: string) => text.trim().toLowerCase().replace(/\s+/g, "_");

export function statusTone(text: string): Tone {
  return TONES[key(text)] ?? "neutral";
}

const UPPER = new Set(["ok", "gst", "lut", "irn", "upi"]);

/** "requires_action" → "Requires action"; "ok" → "OK". */
export function statusLabel(text: string): string {
  const words = key(text).split("_").filter(Boolean);
  return words
    .map((w, i) =>
      UPPER.has(w)
        ? w.toUpperCase()
        : i === 0
          ? w[0].toUpperCase() + w.slice(1)
          : w,
    )
    .join(" ");
}
