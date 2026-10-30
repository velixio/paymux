export type WebhookHeaders = Headers | Record<string, string | string[] | undefined>;
export interface WebhookEvent<T = unknown> { id: string; type: string; timestamp: string; livemode: boolean; data: { object: T } }
export interface WebhookVerifierOptions { toleranceMs?: number; now?: () => number }

export class WebhookError extends Error {
  constructor(readonly type: "headers" | "timestamp" | "signature" | "payload", message: string) { super(message); this.name = "WebhookError"; }
}

function header(headers: WebhookHeaders, name: string): string {
  if (headers instanceof Headers) return headers.get(name) ?? "";
  const key = Object.keys(headers).find(key => key.toLowerCase() === name);
  const value = key ? headers[key] : undefined;
  return Array.isArray(value) ? value.join(" ") : value ?? "";
}
function base64(value: string): Uint8Array {
  try {
    const raw = globalThis.atob(value);
    return Uint8Array.from(raw, character => character.charCodeAt(0));
  } catch { throw new WebhookError("signature", "paymux: webhook secret or signature is not valid base64"); }
}
function source(bytes: Uint8Array): ArrayBuffer { return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer; }

/** Verifies Standard Webhooks signatures against the exact raw request bytes. */
export class WebhookVerifier {
  private readonly key: Promise<CryptoKey>;
  private readonly toleranceMs: number;
  private readonly now: () => number;

  constructor(secret: string, options: WebhookVerifierOptions = {}) {
    const encoded = secret.startsWith("whsec_") ? secret.slice(6) : secret;
    const key = base64(encoded); if (!key.byteLength) throw new WebhookError("signature", "paymux: webhook secret is empty");
    this.toleranceMs = options.toleranceMs ?? 300_000;
    if (this.toleranceMs <= 0 || this.toleranceMs > 3_600_000) throw new RangeError("paymux: webhook tolerance must be between zero and one hour");
    this.now = options.now ?? Date.now;
    this.key = crypto.subtle.importKey("raw", source(key), { name: "HMAC", hash: "SHA-256" }, false, ["verify"]);
  }

  async verify<T = unknown>(headers: WebhookHeaders, rawBody: string | Uint8Array): Promise<WebhookEvent<T>> {
    const id = header(headers, "webhook-id"), timestamp = header(headers, "webhook-timestamp"), signatures = header(headers, "webhook-signature");
    if (!id || !timestamp || !signatures) throw new WebhookError("headers", "paymux: missing webhook headers");
    if (!/^-?\d+$/.test(timestamp) || Math.abs(this.now() - Number(timestamp) * 1000) > this.toleranceMs) throw new WebhookError("timestamp", "paymux: webhook timestamp is invalid or outside tolerance");
    const body = typeof rawBody === "string" ? new TextEncoder().encode(rawBody) : rawBody;
    const prefix = new TextEncoder().encode(`${id}.${timestamp}.`);
    const signed = new Uint8Array(prefix.byteLength + body.byteLength); signed.set(prefix); signed.set(body, prefix.byteLength);
    const key = await this.key;
    let valid = false;
    for (const item of signatures.trim().split(/\s+/)) {
      const [version, encoded] = item.split(",", 2); if (version !== "v1" || !encoded) continue;
      try { if (await crypto.subtle.verify("HMAC", key, source(base64(encoded)), source(signed))) { valid = true; break; } } catch { /* try the next signature */ }
    }
    if (!valid) throw new WebhookError("signature", "paymux: webhook signature does not match");
    let payload: any; try { payload = JSON.parse(new TextDecoder().decode(body)); } catch { throw new WebhookError("payload", "paymux: webhook body is not valid JSON"); }
    if (!payload?.type || payload?.data?.object === undefined) throw new WebhookError("payload", "paymux: webhook event is missing type or data.object");
    return { ...payload, id } as WebhookEvent<T>;
  }
}
