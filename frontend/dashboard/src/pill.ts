export type Tone = "ok" | "warn" | "bad" | "neutral";

const TONES: Record<string, Tone> = {};
for (const w of [
  "succeeded",
  "paid",
  "active",
  "completed",
  "done",
  "registered",
])
  TONES[w] = "ok";
for (const w of [
  "failed",
  "refunded",
  "disputed",
  "lost",
  "canceled",
  "cancelled",
])
  TONES[w] = "bad";
for (const w of [
  "pending",
  "processing",
  "past_due",
  "requires_action",
  "needs_response",
])
  TONES[w] = "warn";

export function statusTone(text: string): Tone {
  return TONES[text.trim().toLowerCase()] ?? "neutral";
}
