import { describe, expect, test } from "bun:test";
import { APIError, Paymux, WebhookError, WebhookVerifier } from "../src/index";

test("payment mutations require idempotency and send credentials only to the configured origin", async () => {
  let request: Request | undefined;
  const client = new Paymux({ apiKey: "pk_test_secret", baseURL: "https://pay.example.com/base", fetch: async (input, init) => {
    request = new Request(input, init);
    return Response.json({ id: "pay_1", status: "created" });
  } });
  await expect(client.payments.create({ amount: 49900, currency: "INR" }, {} as any)).rejects.toThrow("idempotencyKey");
  const payment = await client.payments.create({ amount: 49900, currency: "INR" }, { idempotencyKey: "order-1" });
  expect(payment.id).toBe("pay_1");
  expect(request?.url).toBe("https://pay.example.com/base/v1/payments");
  expect(request?.headers.get("Authorization")).toBe("Bearer pk_test_secret");
  expect(request?.headers.get("Idempotency-Key")).toBe("order-1");
});

test("resource paths, query parameters, and PDF downloads are normalized", async () => {
  const requests: Request[] = [];
  const client = new Paymux({ apiKey: "key", baseURL: "http://localhost:8080", fetch: async (input, init) => {
    const request = new Request(input, init); requests.push(request);
    if (request.headers.get("Accept") === "application/pdf") return new Response(new Uint8Array([1, 2, 3]));
    return Response.json({ object: "list", data: [], has_more: false });
  } });
  await client.payments.list({ limit: 20, status: "succeeded", starting_after: "pay_1" });
  expect(requests[0].url).toContain("limit=20"); expect(requests[0].url).toContain("status=succeeded");
  expect(await client.invoices.downloadPDF("in_1")).toEqual(new Uint8Array([1, 2, 3]));
  expect(requests[1].url).toBe("http://localhost:8080/v1/invoices/in_1/pdf");
  expect(() => client.refunds.get("bad/id")).toThrow("invalid object ID");
  expect(() => client.refunds.get("..")).toThrow("invalid object ID");
  expect(() => client.refunds.get(".")).toThrow("invalid object ID");
});

test("API errors retain safe retry and request metadata", async () => {
  const client = new Paymux({ apiKey: "key", fetch: async () => new Response(JSON.stringify({ error: { type: "rate_limited", message: "Slow down", request_id: "req_1" } }), { status: 429, headers: { "Retry-After": "3", "Idempotent-Replay": "true" } }) });
  try { await client.payments.get("pay_1"); throw new Error("expected API error"); }
  catch (error) { expect(error).toBeInstanceOf(APIError); expect(error).toMatchObject({ type: "rate_limited", requestId: "req_1", temporary: true, retryAfterMs: 3000, idempotentReplay: true }); }
});

describe("WebhookVerifier", () => {
  test("rejects non-finite timestamp tolerances", () => {
    const secret = `whsec_${Buffer.from("test-key").toString("base64")}`;
    expect(() => new WebhookVerifier(secret, { toleranceMs: Number.NaN })).toThrow(RangeError);
    expect(() => new WebhookVerifier(secret, { toleranceMs: Number.POSITIVE_INFINITY })).toThrow(RangeError);
  });

  test("verifies the exact raw Standard Webhooks body", async () => {
    const key = crypto.getRandomValues(new Uint8Array(32));
    const secret = `whsec_${Buffer.from(key).toString("base64")}`;
    const body = JSON.stringify({ type: "payment.succeeded", timestamp: "2026-01-01T00:00:00Z", livemode: false, data: { object: { id: "pay_1" } } });
    const timestamp = "1700000000", id = "evt_1";
    const cryptoKey = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
    const signature = await crypto.subtle.sign("HMAC", cryptoKey, new TextEncoder().encode(`${id}.${timestamp}.${body}`));
    const verifier = new WebhookVerifier(secret, { now: () => Number(timestamp) * 1000 });
    const event = await verifier.verify({ "webhook-id": id, "webhook-timestamp": timestamp, "webhook-signature": `v1,${Buffer.from(signature).toString("base64")}` }, body);
    expect(event.id).toBe(id); expect(event.data.object).toEqual({ id: "pay_1" });
    await expect(verifier.verify({ "webhook-id": id, "webhook-timestamp": timestamp, "webhook-signature": "v1,AAAA" }, body)).rejects.toBeInstanceOf(WebhookError);
  });
});
