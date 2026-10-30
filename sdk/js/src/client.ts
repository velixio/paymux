import type * as T from "./types.js";

export type Fetch = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
export interface PaymuxOptions { apiKey: string; baseURL?: string; fetch?: Fetch; userAgent?: string }
export interface RequestOptions { signal?: AbortSignal }
export interface MutationOptions extends RequestOptions { idempotencyKey: string }

export class APIError extends Error {
  constructor(
    readonly statusCode: number,
    readonly type: string,
    message: string,
    readonly requestId?: string,
    readonly idempotentReplay = false,
    readonly retryAfterMs = 0,
  ) { super(message); this.name = "APIError"; }
  get temporary(): boolean { return this.statusCode === 429 || this.statusCode >= 500; }
  get isNotFound(): boolean { return this.statusCode === 404; }
  get isConflict(): boolean { return this.statusCode === 409; }
  static async fromResponse(response: Response): Promise<APIError> {
    const text = (await response.text()).slice(0, 1 << 20);
    let value: any; try { value = JSON.parse(text); } catch { /* plain error */ }
    const retry = response.headers.get("Retry-After") ?? "";
    const seconds = /^\d+$/.test(retry) ? Number(retry) * 1000 : Math.max(0, Date.parse(retry) - Date.now()) || 0;
    const message = value?.error?.message ?? (text.trim() || response.statusText);
    return new APIError(response.status, value?.error?.type ?? response.statusText, message,
      value?.error?.request_id ?? response.headers.get("X-Request-Id") ?? undefined, response.headers.get("Idempotent-Replay")?.toLowerCase() === "true", seconds);
  }
}

function baseURL(raw: string): URL {
  let url: URL;
  try { url = new URL(raw); } catch { throw new Error("paymux: baseURL must be absolute"); }
  if (url.username || url.password || url.search || url.hash) throw new Error("paymux: baseURL must not contain credentials, query, or fragment");
  const local = url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "[::1]";
  if (url.protocol !== "https:" && !(url.protocol === "http:" && local)) throw new Error("paymux: baseURL must use HTTPS (HTTP is allowed only on loopback)");
  url.pathname = `${url.pathname.replace(/\/+$/, "")}/`;
  return url;
}
function id(value: string): string { if (!value || value === "." || value === ".." || value.includes("/")) throw new Error("paymux: invalid object ID"); return encodeURIComponent(value); }
function mutation(options: MutationOptions): void {
  const key = options?.idempotencyKey;
  if (!key || key.trim() !== key || key.length > 255 || /[\r\n]/.test(key)) throw new Error("paymux: mutation requires a 1-255 character idempotencyKey");
}
function query(params: object = {}): URLSearchParams {
  const out = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) if (value !== undefined && value !== "" && value !== 0) out.set(key, String(value));
  return out;
}

export class Paymux {
  private readonly apiKey: string;
  private readonly base: URL;
  private readonly fetcher: Fetch;
  private readonly userAgent: string;

  readonly payments = {
    create: (params: T.CreatePaymentParams, options: MutationOptions) => this.request<T.Payment>("POST", "v1/payments", params, options),
    get: (paymentId: string, options?: RequestOptions) => this.request<T.Payment>("GET", `v1/payments/${id(paymentId)}`, undefined, options),
    list: (params: T.ListPaymentsParams = {}, options?: RequestOptions) => this.request<T.PaymentPage>("GET", "v1/payments", undefined, options, query(params)),
    cancel: (paymentId: string, options: MutationOptions) => this.request<T.Payment>("POST", `v1/payments/${id(paymentId)}/cancel`, undefined, options),
  };
  readonly refunds = {
    create: (paymentId: string, params: T.CreateRefundParams, options: MutationOptions) => this.request<T.Refund>("POST", `v1/payments/${id(paymentId)}/refunds`, params, options),
    get: (refundId: string, options?: RequestOptions) => this.request<T.Refund>("GET", `v1/refunds/${id(refundId)}`, undefined, options),
  };
  readonly paymentLinks = {
    create: (params: T.CreatePaymentLinkParams, options: MutationOptions) => this.request<T.PaymentLink>("POST", "v1/payment_links", params, options),
    get: (linkId: string, options?: RequestOptions) => this.request<T.PaymentLink>("GET", `v1/payment_links/${id(linkId)}`, undefined, options),
    list: (params: T.ListPaymentLinksParams = {}, options?: RequestOptions) => this.request<T.PaymentLinkPage>("GET", "v1/payment_links", undefined, options, query(params)),
    deactivate: (linkId: string, options: MutationOptions) => this.request<T.PaymentLink>("POST", `v1/payment_links/${id(linkId)}/deactivate`, undefined, options),
  };
  readonly subscriptions = {
    create: (params: T.CreateSubscriptionParams, options: MutationOptions) => this.request<T.Subscription>("POST", "v1/subscriptions", params, options),
    get: (subscriptionId: string, options?: RequestOptions) => this.request<T.Subscription>("GET", `v1/subscriptions/${id(subscriptionId)}`, undefined, options),
    list: (params: T.ListSubscriptionsParams = {}, options?: RequestOptions) => this.request<T.SubscriptionPage>("GET", "v1/subscriptions", undefined, options, query(params)),
    cancel: (subscriptionId: string, params: T.CancelSubscriptionParams, options: MutationOptions) => this.request<T.Subscription>("POST", `v1/subscriptions/${id(subscriptionId)}/cancel`, params, options),
    update: (subscriptionId: string, params: T.UpdateSubscriptionParams, options: MutationOptions) => this.request<T.Subscription>("POST", `v1/subscriptions/${id(subscriptionId)}/update`, params, options),
    updatePaymentMethod: (subscriptionId: string, params: T.UpdatePaymentMethodParams, options: MutationOptions) => this.request<T.Subscription>("POST", `v1/subscriptions/${id(subscriptionId)}/payment_method`, params, options),
  };
  readonly invoices = {
    list: (paymentId: string, options?: RequestOptions) => this.requiredList<T.InvoicePage>("invoices", paymentId, options),
    get: (invoiceId: string, options?: RequestOptions) => this.request<T.Invoice>("GET", `v1/invoices/${id(invoiceId)}`, undefined, options),
    downloadPDF: (invoiceId: string, options?: RequestOptions) => this.pdf("invoices", invoiceId, options),
  };
  readonly creditNotes = {
    list: (paymentId: string, options?: RequestOptions) => this.requiredList<T.CreditNotePage>("credit_notes", paymentId, options),
    get: (creditNoteId: string, options?: RequestOptions) => this.request<T.CreditNote>("GET", `v1/credit_notes/${id(creditNoteId)}`, undefined, options),
    downloadPDF: (creditNoteId: string, options?: RequestOptions) => this.pdf("credit_notes", creditNoteId, options),
  };
  readonly payouts = {
    create: (params: T.CreatePayoutParams, options: MutationOptions) => this.request<T.Payout>("POST", "v1/payouts", params, options),
    get: (payoutId: string, options?: RequestOptions) => this.request<T.Payout>("GET", `v1/payouts/${id(payoutId)}`, undefined, options),
    list: (params: T.ListPayoutsParams = {}, options?: RequestOptions) => this.request<T.PayoutPage>("GET", "v1/payouts", undefined, options, query(params)),
  };

  constructor(options: PaymuxOptions) {
    if (!options?.apiKey?.trim() || /[\r\n]/.test(options.apiKey)) throw new Error("paymux: valid apiKey is required");
    if (options.userAgent && /[\r\n]/.test(options.userAgent)) throw new Error("paymux: invalid userAgent");
    this.apiKey = options.apiKey; this.base = baseURL(options.baseURL ?? "http://localhost:8080");
    this.fetcher = options.fetch ?? globalThis.fetch; this.userAgent = `paymux-js/0.1${options.userAgent ? ` ${options.userAgent.trim()}` : ""}`;
  }

  private async request<T>(method: string, path: string, body?: unknown, options?: RequestOptions | MutationOptions, params?: URLSearchParams): Promise<T> {
    if (method === "POST") mutation(options as MutationOptions);
    const url = new URL(path, this.base); if (params) url.search = params.toString();
    const headers: Record<string, string> = { Authorization: `Bearer ${this.apiKey}`, Accept: "application/json", "User-Agent": this.userAgent };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (method === "POST") headers["Idempotency-Key"] = (options as MutationOptions).idempotencyKey;
    const response = await this.fetcher(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal: options?.signal });
    if (!response.ok) throw await APIError.fromResponse(response);
    try { return await response.json() as T; } catch { throw new Error("paymux: response is not valid JSON"); }
  }
  private requiredList<T>(collection: string, paymentId: string, options?: RequestOptions): Promise<T> {
    if (!paymentId) throw new Error("paymux: payment ID is required");
    return this.request<T>("GET", `v1/${collection}`, undefined, options, query({ payment_id: paymentId }));
  }
  private async pdf(collection: string, objectId: string, options?: RequestOptions): Promise<Uint8Array> {
    const url = new URL(`v1/${collection}/${id(objectId)}/pdf`, this.base);
    const response = await this.fetcher(url, { headers: { Authorization: `Bearer ${this.apiKey}`, Accept: "application/pdf", "User-Agent": this.userAgent }, signal: options?.signal });
    if (!response.ok) throw await APIError.fromResponse(response);
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.byteLength > 32 << 20) throw new Error("paymux: PDF exceeds 32 MiB limit");
    return bytes;
  }
}

