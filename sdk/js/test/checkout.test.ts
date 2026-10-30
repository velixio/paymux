import { describe, expect, test } from "bun:test";
import { Window as DOMWindow } from "happy-dom";
import { CheckoutError, checkout, executeCheckout, parseCheckoutURL } from "../src/checkout";

describe("parseCheckoutURL", () => {
  test("extracts an opaque token without retaining it in the endpoint", () => {
    const parsed = parseCheckoutURL("https://pay.example.com/pay/pay_123?t=secret-value");
    expect(parsed).toEqual({ endpoint: "https://pay.example.com/pay/pay_123/action", token: "secret-value" });
    expect(parsed.endpoint).not.toContain("secret-value");
  });

  test("rejects non-Paymux and insecure remote URLs", () => {
    expect(() => parseCheckoutURL("https://pay.example.com/other?t=x")).toThrow(CheckoutError);
    expect(() => parseCheckoutURL("http://pay.example.com/pay/pay_1?t=x")).toThrow("HTTPS");
    expect(parseCheckoutURL("http://localhost:8080/pay/pay_1?t=x").endpoint).toBe("http://localhost:8080/pay/pay_1/action");
  });
});

test("redirect action sends the token only in its header", async () => {
  let requestedURL = "";
  let requestedInit: RequestInit | undefined;
  let assigned = "";
  const result = await checkout({
    checkoutUrl: "https://pay.example.com/pay/pay_123?t=top-secret",
    ui: false,
    document: {} as Document,
    window: { location: { href: "https://shop.example.com/order", assign: (url: string) => { assigned = url; } } } as unknown as Window,
    fetch: async (input, init) => {
      requestedURL = String(input); requestedInit = init;
      return new Response(JSON.stringify({ kind: "checkout", payment_id: "pay_123", status: "created", action: { kind: "redirect", url: "https://checkout.stripe.com/c/pay/test" } }), { status: 200, headers: { "Content-Type": "application/json" } });
    },
  });
  expect(requestedURL).toBe("https://pay.example.com/pay/pay_123/action");
  expect(requestedURL).not.toContain("top-secret");
  expect(new Headers(requestedInit?.headers).get("Paymux-Checkout-Token")).toBe("top-secret");
  expect(requestedInit?.credentials).toBe("omit");
  expect(assigned).toBe("https://checkout.stripe.com/c/pay/test");
  expect(result).toEqual({ status: "redirecting", paymentId: "pay_123" });
});

test("backend-resolved action opens without a Paymux URL or request", async () => {
  let assigned = "";
  const result = await executeCheckout({
    kind: "checkout",
    payment_id: "pay_direct",
    status: "created",
    launcher: "stripe",
    action: { kind: "redirect", url: "https://checkout.stripe.com/c/pay/direct" },
  }, {
    document: {} as Document,
    window: { location: { assign: (url: string) => { assigned = url; } } } as unknown as Window,
  });
  expect(assigned).toBe("https://checkout.stripe.com/c/pay/direct");
  expect(result).toEqual({ status: "redirecting", paymentId: "pay_direct" });
});

test("complete payment does not open a provider", async () => {
  const events: string[] = [];
  const result = await checkout({
    checkoutUrl: "https://pay.example.com/pay/pay_done?t=secret",
    ui: false,
    document: {} as Document,
    window: { location: { href: "https://shop.example.com/order", assign: () => { throw new Error("unexpected redirect"); } } } as unknown as Window,
    fetch: async () => new Response(JSON.stringify({ kind: "complete", payment_id: "pay_done", status: "succeeded" }), { status: 200 }),
    onEvent: (event) => events.push(event.type),
  });
  expect(result).toEqual({ status: "complete", paymentId: "pay_done", paymentStatus: "succeeded" });
  expect(events).toEqual(["loading", "complete"]);
});

test("typed API errors are exposed without leaking response bodies", async () => {
  await expect(checkout({
    checkoutUrl: "https://pay.example.com/pay/pay_1?t=secret",
    ui: false,
    document: {} as Document,
    window: { location: { href: "https://shop.example.com" } } as unknown as Window,
    fetch: async () => new Response(JSON.stringify({ error: { type: "origin_not_allowed", message: "Return URL mismatch", request_id: "req_1" } }), { status: 403 }),
  })).rejects.toMatchObject({ name: "CheckoutError", type: "origin_not_allowed", status: 403, requestId: "req_1" });
});

test("form_post creates a hidden POST form with signed fields", async () => {
  const dom = new DOMWindow({ url: "https://shop.example.com/order" });
  let submitted: HTMLFormElement | undefined;
  dom.HTMLFormElement.prototype.submit = function () { submitted = this as unknown as HTMLFormElement; };
  const result = await checkout({
    checkoutUrl: "https://pay.example.com/pay/pay_payu?t=secret",
    ui: false,
    document: dom.document as unknown as Document,
    window: dom as unknown as Window,
    fetch: async () => new Response(JSON.stringify({ kind: "checkout", payment_id: "pay_payu", status: "created", launcher: "payu", action: { kind: "form_post", url: "https://test.payu.in/_payment", fields: { txnid: "txn_1", hash: "signed-value" } } }), { status: 200 }),
  });
  expect(result.status).toBe("redirecting");
  expect(submitted?.method).toBe("post");
  expect(submitted?.action).toBe("https://test.payu.in/_payment");
  expect(submitted?.hidden).toBe(true);
  expect(new dom.FormData(submitted as any).get("hash")).toBe("signed-value");
  dom.close();
});

test("provider SDK action loads and reports modal dismissal", async () => {
  let opened = 0;
  let loaded: (() => void) | undefined;
  const script = { src: "", async: false, referrerPolicy: "", nonce: "", addEventListener(type: string, handler: () => void) { if (type === "load") loaded = handler; } };
  const window = { location: { href: "https://shop.example.com/order" } } as unknown as Window;
  const Razorpay = class {
    constructor(private readonly options: { modal: { ondismiss: () => void } }) {}
    open() { opened++; this.options.modal.ondismiss(); }
  };
  const document = { createElement: () => script, head: { append: () => queueMicrotask(() => { (window as any).Razorpay = Razorpay; loaded?.(); }) } } as unknown as Document;
  const result = await checkout({
    checkoutUrl: "https://pay.example.com/pay/pay_rzp?t=secret",
    ui: false,
    nonce: "request-nonce",
    document,
    window,
    fetch: async () => new Response(JSON.stringify({ kind: "checkout", payment_id: "pay_rzp", status: "created", launcher: "razorpay", action: { kind: "sdk", script_url: "https://checkout.razorpay.com/v1/checkout.js", options: { key: "public-key", order_id: "order_1" } } }), { status: 200 }),
  });
  expect(opened).toBe(1);
  expect(script.nonce).toBe("request-nonce");
  expect(result).toEqual({ status: "dismissed", paymentId: "pay_rzp" });
});

test("provider SDK action reuses an already loaded provider", async () => {
  let created = 0;
  const document = { createElement: () => { created++; return {}; } } as unknown as Document;
  const window = { location: { href: "https://shop.example.com/order" }, Razorpay: class {
    constructor(private readonly options: { modal: { ondismiss: () => void } }) {}
    open() { this.options.modal.ondismiss(); }
  } } as unknown as Window;
  const result = await checkout({ checkoutUrl: "https://pay.example.com/pay/pay_existing?t=secret", ui: false, document, window,
    fetch: async () => Response.json({ kind: "checkout", payment_id: "pay_existing", status: "created", launcher: "razorpay", action: { kind: "sdk", script_url: "https://checkout.razorpay.com/v1/checkout.js", options: {} } }) });
  expect(created).toBe(0);
  expect(result.status).toBe("dismissed");
});

test("failed provider scripts are removed before retry", async () => {
  let failed: (() => void) | undefined;
  let removed = 0;
  const script = { src: "", async: false, referrerPolicy: "", addEventListener(type: string, handler: () => void) { if (type === "error") failed = handler; }, remove() { removed++; } };
  const document = { createElement: () => script, head: { append: () => queueMicrotask(() => failed?.()) } } as unknown as Document;
  await expect(checkout({ checkoutUrl: "https://pay.example.com/pay/pay_failed?t=secret", ui: false, document,
    window: { location: { href: "https://shop.example.com/order" } } as unknown as Window,
    fetch: async () => Response.json({ kind: "checkout", payment_id: "pay_failed", status: "created", launcher: "razorpay", action: { kind: "sdk", script_url: "https://checkout.razorpay.com/v1/checkout.js", options: {} } })
  })).rejects.toMatchObject({ type: "provider_script_failed" });
  expect(removed).toBe(1);
});
