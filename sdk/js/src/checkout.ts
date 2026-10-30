export type CheckoutEvent =
  | { type: "loading" }
  | { type: "billing" }
  | { type: "provider"; provider: string }
  | { type: "redirecting"; url: string }
  | { type: "dismissed" }
  | { type: "complete"; paymentId: string; status: string }
  | { type: "error"; error: CheckoutError };

export type CheckoutResult =
  | { status: "redirecting"; paymentId: string }
  | { status: "opened"; paymentId: string }
  | { status: "dismissed"; paymentId: string }
  | { status: "complete"; paymentId: string; paymentStatus: string };

export interface CheckoutOptions {
  /** The customer checkout_url returned by the Paymux backend API. */
  checkoutUrl: string;
  /** Start another provider attempt instead of reusing an open one. */
  fresh?: boolean;
  /** Built-in accessible loading, billing, error, and retry UI. Default true. */
  ui?: boolean;
  /** CSP nonce applied to the built-in UI style and dynamically loaded provider scripts. */
  nonce?: string;
  /** Receives lifecycle events. Browser events are never proof of payment. */
  onEvent?: (event: CheckoutEvent) => void;
  signal?: AbortSignal;
  /** Test/server adapters; ordinary browser integrations do not set these. */
  fetch?: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
  window?: Window;
  document?: Document;
}

export class CheckoutError extends Error {
  readonly type: string;
  readonly status: number;
  readonly requestId?: string;

  constructor(type: string, message: string, status = 0, requestId?: string) {
    super(message);
    this.name = "CheckoutError";
    this.type = type;
    this.status = status;
    this.requestId = requestId;
  }
}

export interface ProviderAction {
  kind: "redirect" | "form_post" | "sdk";
  url?: string;
  fields?: Record<string, string>;
  script_url?: string;
  options?: Record<string, unknown>;
}

interface BillingValues {
  name: string;
  email: string;
  country: string;
  state_code: string;
  line1: string;
  postal_code: string;
  gstin: string;
  legal_name: string;
}

interface BillingAction {
  values: BillingValues;
  states?: Array<{ code: string; name: string }>;
  state_required: boolean;
  address_required: boolean;
  country_required: boolean;
}

export interface ActionResponse {
  kind: "billing" | "checkout" | "complete";
  payment_id: string;
  status: string;
  launcher?: string;
  action?: ProviderAction;
  billing?: BillingAction;
}

export interface ExecuteCheckoutOptions {
	/** CSP nonce applied to dynamically loaded provider scripts. */
	nonce?: string;
	onEvent?: (event: CheckoutEvent) => void;
	window?: Window;
	document?: Document;
}

interface ParsedCheckout {
  endpoint: string;
  token: string;
}

export function parseCheckoutURL(raw: string, pageURL?: string): ParsedCheckout {
  let url: URL;
  try {
    url = new URL(raw, pageURL);
  } catch {
    throw new CheckoutError("invalid_checkout_url", "The checkout URL is invalid.");
  }
  const match = url.pathname.match(/^\/pay\/([^/]+)$/);
  const token = url.searchParams.get("t") ?? "";
  if (!match || !token) {
    throw new CheckoutError("invalid_checkout_url", "The checkout URL is not a Paymux payment session.");
  }
  const local = url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "[::1]";
  if (url.protocol !== "https:" && !(url.protocol === "http:" && local)) {
    throw new CheckoutError("insecure_checkout_url", "Checkout must use HTTPS outside local development.");
  }
  let paymentID = "";
  try { paymentID = decodeURIComponent(match[1]); } catch { throw new CheckoutError("invalid_checkout_url", "The checkout URL contains an invalid payment ID."); }
  if (!/^[A-Za-z0-9_-]+$/.test(paymentID)) throw new CheckoutError("invalid_checkout_url", "The checkout URL contains an invalid payment ID.");
  return { endpoint: `${url.origin}/pay/${encodeURIComponent(paymentID)}/action`, token };
}

class CheckoutUI {
  private host: HTMLElement;
  private shadow: ShadowRoot;
  private previousFocus: Element | null;

  constructor(private readonly doc: Document, nonce?: string) {
    this.previousFocus = doc.activeElement;
    this.host = doc.createElement("div");
    this.host.setAttribute("data-paymux-checkout", "");
    this.shadow = this.host.attachShadow({ mode: "closed" });
    const style = doc.createElement("style");
    if (nonce) style.nonce = nonce;
    style.textContent = `
      :host{all:initial} .backdrop{position:fixed;inset:0;z-index:2147483646;display:grid;place-items:center;padding:16px;background:rgba(15,23,42,.58);font:16px/1.5 system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;color:#172033}
      .card{box-sizing:border-box;width:min(100%,480px);max-height:min(90vh,760px);overflow:auto;border-radius:16px;background:#fff;padding:24px;box-shadow:0 24px 64px rgba(15,23,42,.3)}
      h2{margin:0 0 8px;font-size:22px;line-height:1.25} p{margin:0 0 16px;color:#475569}.row{display:grid;gap:6px;margin:0 0 14px}label{font-weight:600;font-size:14px}
      input,select{box-sizing:border-box;width:100%;min-height:44px;border:1px solid #94a3b8;border-radius:8px;padding:10px 12px;background:#fff;color:#172033;font:inherit}
      input:focus,select:focus,button:focus{outline:3px solid #2563eb;outline-offset:2px}.actions{display:flex;flex-wrap:wrap;justify-content:flex-end;gap:10px;margin-top:20px}
      button{min-width:96px;min-height:44px;border:0;border-radius:8px;padding:10px 16px;background:#172033;color:#fff;font:600 15px/1.2 inherit;cursor:pointer;touch-action:manipulation}button:hover{background:#29364d}button:active{background:#0f172a}button.secondary{background:#e2e8f0;color:#172033}button.secondary:hover{background:#cbd5e1}button.secondary:active{background:#b8c4d3}
      .spinner{width:28px;height:28px;margin:0 auto 16px;border:3px solid #cbd5e1;border-top-color:#2563eb;border-radius:50%;animation:spin .8s linear infinite}@keyframes spin{to{transform:rotate(360deg)}}
      details{margin-top:8px}summary{min-height:44px;display:flex;align-items:center;cursor:pointer;font-weight:600}.error{color:#b42318}
      @media(prefers-reduced-motion:reduce){.spinner{animation:none}}
      @media(prefers-color-scheme:dark){.card{background:#111827;color:#f8fafc}.card p{color:#d1d5db}input,select{background:#1f2937;color:#f8fafc;border-color:#94a3b8}button{background:#e2e8f0;color:#111827}button:hover{background:#f1f5f9}button:active{background:#cbd5e1}button.secondary{background:#334155;color:#f8fafc}button.secondary:hover{background:#475569}button.secondary:active{background:#1e293b}.error{color:#ffb4ab}}
      @supports(padding:max(0px)){.backdrop{padding:max(16px,env(safe-area-inset-top)) max(16px,env(safe-area-inset-right)) max(16px,env(safe-area-inset-bottom)) max(16px,env(safe-area-inset-left))}}
    `;
    this.shadow.innerHTML = `<div class="backdrop"><section class="card" role="dialog" aria-modal="true" aria-labelledby="pmx-title" tabindex="-1"><div id="content"></div></section></div>`;
    this.shadow.prepend(style);
    this.shadow.addEventListener("keydown", (event) => {
      if (!(event instanceof KeyboardEvent)) return;
      if (event.key === "Escape") {
        const close = this.shadow.getElementById("cancel") ?? this.shadow.getElementById("close");
        if (close instanceof HTMLElement) { event.preventDefault(); close.click(); }
        return;
      }
      if (event.key !== "Tab") return;
      const focusable = [...this.shadow.querySelectorAll<HTMLElement>("button:not([disabled]),input:not([disabled]),select:not([disabled]),summary,[tabindex]:not([tabindex='-1'])")];
      if (!focusable.length) { event.preventDefault(); (this.shadow.querySelector(".card") as HTMLElement).focus(); return; }
      const first = focusable[0], last = focusable[focusable.length - 1];
      if (event.shiftKey && this.shadow.activeElement === first) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && this.shadow.activeElement === last) { event.preventDefault(); first.focus(); }
    });
    doc.body.append(this.host);
  }

  private content(): HTMLElement {
    return this.shadow.getElementById("content") as HTMLElement;
  }

  loading(message = "Opening secure checkout…"): void {
    const content = this.content();
    content.setAttribute("role", "status");
    content.setAttribute("aria-live", "polite");
    content.setAttribute("aria-busy", "true");
    content.innerHTML = `<div class="spinner" aria-hidden="true"></div><h2 id="pmx-title">Secure checkout</h2><p id="pmx-message"></p>`;
    (this.shadow.getElementById("pmx-message") as HTMLElement).textContent = message;
    (this.shadow.querySelector(".card") as HTMLElement).focus();
  }

  async error(error: CheckoutError): Promise<boolean> {
    const content = this.content();
    content.removeAttribute("aria-busy");
    content.innerHTML = `<div role="alert"><h2 id="pmx-title">Checkout could not open</h2><p class="error" id="pmx-error"></p></div><div class="actions"><button class="secondary" id="close" type="button">Close</button><button id="retry" type="button">Try again</button></div>`;
    (this.shadow.getElementById("pmx-error") as HTMLElement).textContent = error.message;
    return new Promise((resolve) => {
      const done = (retry: boolean) => resolve(retry);
      this.shadow.getElementById("retry")?.addEventListener("click", () => done(true), { once: true });
      this.shadow.getElementById("close")?.addEventListener("click", () => done(false), { once: true });
      (this.shadow.getElementById("retry") as HTMLElement).focus();
    });
  }

  async billing(model: BillingAction, error?: CheckoutError): Promise<BillingValues | null> {
    const content = this.content();
    content.removeAttribute("aria-busy");
    content.innerHTML = `<h2 id="pmx-title">Billing details</h2><p>Enter the details required for this payment.</p><form id="billing" novalidate><div id="billing-error"></div><div id="fields"></div><details><summary>GST details (optional)</summary><div id="gst"></div></details><div class="actions"><button class="secondary" id="cancel" type="button">Cancel</button><button type="submit">Continue</button></div></form>`;
    const fields = this.shadow.getElementById("fields") as HTMLElement;
    const gst = this.shadow.getElementById("gst") as HTMLElement;
    const addInput = (parent: HTMLElement, name: keyof BillingValues, label: string, required = false, attrs: Record<string, string> = {}) => {
      const row = this.doc.createElement("div"); row.className = "row";
      const lab = this.doc.createElement("label"); lab.htmlFor = `pmx-${name}`; lab.textContent = label;
      const input = this.doc.createElement("input"); input.id = `pmx-${name}`; input.name = name; input.value = model.values[name] ?? ""; input.required = required;
      for (const [key, value] of Object.entries(attrs)) input.setAttribute(key, value);
      row.append(lab, input); parent.append(row);
    };
    addInput(fields, "name", "Name", true, { autocomplete: "name" });
    addInput(fields, "email", "Email", false, { type: "email", autocomplete: "email" });
    addInput(fields, "country", "Country code", model.country_required, { maxlength: "2", autocomplete: "country" });
    if (model.states?.length) {
      const row = this.doc.createElement("div"); row.className = "row";
      const label = this.doc.createElement("label"); label.htmlFor = "pmx-state_code"; label.textContent = "State";
      const select = this.doc.createElement("select"); select.id = "pmx-state_code"; select.name = "state_code"; select.required = model.state_required;
      const blank = this.doc.createElement("option"); blank.value = ""; blank.textContent = "Select a state"; select.append(blank);
      for (const state of model.states) { const option = this.doc.createElement("option"); option.value = state.code; option.textContent = state.name; option.selected = state.code === model.values.state_code; select.append(option); }
      row.append(label, select); fields.append(row);
    } else addInput(fields, "state_code", "State code", model.state_required);
    if (model.address_required) {
      addInput(fields, "line1", "Address", true, { autocomplete: "address-line1" });
      addInput(fields, "postal_code", "PIN code", true, { autocomplete: "postal-code", inputmode: "numeric" });
    }
    addInput(gst, "gstin", "GSTIN"); addInput(gst, "legal_name", "Legal name");
    let errorTarget: HTMLElement | null = null;
    if (error) {
      const fieldByError: Record<string, keyof BillingValues> = {
        "billing.err.name": "name", "billing.err.email": "email", "billing.err.country": "country", "billing.err.country_required": "country",
        "billing.err.state_invalid": "state_code", "billing.err.state_required": "state_code", "billing.err.gstin": "gstin",
        "billing.err.legal_name": "legal_name", "billing.err.gst_state": "gstin", "billing.err.address": "line1", "billing.err.pin": "postal_code",
      };
      const alert = this.shadow.getElementById("billing-error") as HTMLElement; alert.className = "error"; alert.setAttribute("role", "alert"); alert.tabIndex = -1; alert.textContent = error.message;
      const field = fieldByError[error.type];
      errorTarget = field ? this.shadow.getElementById(`pmx-${field}`) as HTMLElement | null : alert;
      if (field && errorTarget) {
        const inline = this.doc.createElement("p"); inline.className = "error"; inline.id = `pmx-${field}-error`; inline.textContent = error.message;
        errorTarget.setAttribute("aria-invalid", "true"); errorTarget.setAttribute("aria-describedby", inline.id); errorTarget.parentElement?.append(inline);
      }
    }
    const form = this.shadow.getElementById("billing") as HTMLFormElement;
    return new Promise((resolve) => {
      this.shadow.getElementById("cancel")?.addEventListener("click", () => resolve(null), { once: true });
      form.addEventListener("submit", (event) => {
        event.preventDefault();
        if (!form.reportValidity()) return;
        const data = new FormData(form);
        const value = {} as BillingValues;
        for (const name of ["name", "email", "country", "state_code", "line1", "postal_code", "gstin", "legal_name"] as const) value[name] = String(data.get(name) ?? "").trim();
        resolve(value);
      });
      (errorTarget ?? this.shadow.getElementById("pmx-name") as HTMLElement).focus();
    });
  }

  destroy(): void {
    this.host.remove();
    if (this.previousFocus instanceof HTMLElement) this.previousFocus.focus();
  }

  detach(): void { this.host.remove(); }
  attach(): void { if (!this.host.isConnected) this.doc.body.append(this.host); }
}

function checkoutError(value: unknown): CheckoutError {
  if (value instanceof CheckoutError) return value;
  if (value instanceof DOMException && value.name === "AbortError") return new CheckoutError("aborted", "Checkout was canceled.");
  return new CheckoutError("network_error", "Checkout could not connect. Check your connection and try again.");
}

async function requestAction(parsed: ParsedCheckout, options: CheckoutOptions, billing?: BillingValues): Promise<ActionResponse> {
  const fetcher = options.fetch ?? globalThis.fetch;
  const response = await fetcher(parsed.endpoint, {
    method: "POST", mode: "cors", credentials: "omit", cache: "no-store", referrerPolicy: "no-referrer", signal: options.signal,
    headers: { "Content-Type": "application/json", "Paymux-Checkout-Token": parsed.token },
    body: JSON.stringify({ fresh: options.fresh === true, ...(billing ? { billing } : {}) }),
  });
  const value = await response.json().catch(() => null) as ActionResponse & { error?: { type?: string; message?: string; request_id?: string } } | null;
  if (!response.ok || !value) {
    throw new CheckoutError(value?.error?.type ?? "checkout_error", value?.error?.message ?? `Checkout failed with HTTP ${response.status}.`, response.status, value?.error?.request_id);
  }
  return value;
}

function safeProviderURL(raw: string | undefined, kind: string): string {
  if (!raw) throw new CheckoutError("invalid_action", `Provider ${kind} action has no URL.`);
  const url = new URL(raw);
  const local = url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "[::1]";
  if (url.protocol !== "https:" && !(url.protocol === "http:" && local)) throw new CheckoutError("invalid_action", `Provider ${kind} URL is not HTTPS.`);
  return url.toString();
}

const documentScripts = new WeakMap<Document, Map<string, Promise<void>>>();
function loadScript(doc: Document, raw: string | undefined, nonce?: string, alreadyLoaded?: () => boolean): Promise<void> {
  const url = safeProviderURL(raw, "script");
  if (alreadyLoaded?.()) return Promise.resolve();
  let scripts = documentScripts.get(doc);
  if (!scripts) { scripts = new Map(); documentScripts.set(doc, scripts); }
  const existing = scripts.get(url);
  if (existing) return existing;
  const loaded = new Promise<void>((resolve, reject) => {
    const script = doc.createElement("script"); script.src = url; script.async = true; script.referrerPolicy = "no-referrer";
    if (nonce) script.nonce = nonce;
    script.addEventListener("load", () => resolve(), { once: true });
    script.addEventListener("error", () => { script.remove(); reject(new CheckoutError("provider_script_failed", "The payment provider could not be loaded.")); }, { once: true });
    doc.head.append(script);
  });
  scripts.set(url, loaded); loaded.catch(() => scripts.delete(url));
  return loaded;
}

function submitForm(doc: Document, action: ProviderAction): void {
  const form = doc.createElement("form"); form.method = "post"; form.action = safeProviderURL(action.url, "form"); form.hidden = true;
  for (const [name, value] of Object.entries(action.fields ?? {})) { const input = doc.createElement("input"); input.type = "hidden"; input.name = name; input.value = value; form.append(input); }
  doc.body.append(form); form.submit();
}

async function openProvider(win: Window, doc: Document, launcher: string, action: ProviderAction, nonce?: string): Promise<"opened" | "dismissed"> {
  const globals = win as unknown as Record<string, any>;
  const providerGlobal = launcher === "razorpay" ? "Razorpay" : launcher === "cashfree" ? "Cashfree" : launcher === "zoho" ? "ZPayments" : "";
  await loadScript(doc, action.script_url, nonce, () => providerGlobal !== "" && typeof globals[providerGlobal] === "function");
  const providerOptions = { ...(action.options ?? {}) } as Record<string, any>;
  if (launcher === "razorpay") {
    if (typeof globals.Razorpay !== "function") throw new CheckoutError("provider_script_failed", "Razorpay Checkout is unavailable.");
    return new Promise((resolve) => {
      const priorModal = typeof providerOptions.modal === "object" && providerOptions.modal ? providerOptions.modal : {};
      providerOptions.modal = { ...priorModal, ondismiss: () => { priorModal.ondismiss?.(); resolve("dismissed"); } };
      new globals.Razorpay(providerOptions).open();
    });
  }
  if (launcher === "cashfree") {
    if (typeof globals.Cashfree !== "function") throw new CheckoutError("provider_script_failed", "Cashfree Checkout is unavailable.");
    const cashfree = globals.Cashfree({ mode: providerOptions.mode });
    const result = await cashfree.checkout({ paymentSessionId: providerOptions.payment_session_id, redirectTarget: "_self" });
    if (result?.error) throw new CheckoutError("provider_error", result.error.message ?? "Cashfree could not open checkout.");
    return "opened";
  }
  if (launcher === "zoho") {
    if (typeof globals.ZPayments !== "function") throw new CheckoutError("provider_script_failed", "Zoho Payments is unavailable.");
    const widget = new globals.ZPayments({ account_id: providerOptions.account_id, domain: providerOptions.domain, otherOptions: { api_key: providerOptions.api_key, is_test_mode: providerOptions.is_test_mode === true } });
    try {
      const data = await widget.requestPaymentMethod({ amount: providerOptions.amount, currency_code: providerOptions.currency, payments_session_id: providerOptions.payments_session_id, description: providerOptions.description });
      const back = new URL(safeProviderURL(String(providerOptions.return_url), "return")); back.searchParams.set("payments_session_id", String(providerOptions.payments_session_id));
      const id = data?.payment_id ?? data?.paymentId; if (id) back.searchParams.set("payment_id", String(id));
      win.location.assign(back.toString()); return "opened";
    } catch (error: any) {
      if (error?.code === "widget_closed") return "dismissed";
      if (error instanceof CheckoutError) throw error;
      throw new CheckoutError("provider_error", error?.message ?? "Zoho Payments could not open checkout.");
    } finally { try { widget.close(); } catch { /* provider cleanup */ } }
  }
  throw new CheckoutError("unsupported_provider", `The checkout SDK does not support provider ${launcher}.`);
}

export async function checkout(options: CheckoutOptions): Promise<CheckoutResult> {
  const win = options.window ?? globalThis.window;
  const doc = options.document ?? globalThis.document;
  if (!win || !doc) throw new CheckoutError("browser_required", "Checkout must run in a browser.");
  const parsed = parseCheckoutURL(options.checkoutUrl, win.location.href);
  const ui = options.ui === false ? null : new CheckoutUI(doc, options.nonce);
  const emit = (event: CheckoutEvent) => options.onEvent?.(event);
  let billing: BillingValues | undefined;
  let billingModel: BillingAction | undefined;
  let billingPaymentID = "";
  try {
    for (;;) {
      ui?.loading(); emit({ type: "loading" });
      let response: ActionResponse;
      try { response = await requestAction(parsed, options, billing); }
      catch (raw) {
        const error = checkoutError(raw); emit({ type: "error", error });
        if (ui && billing && billingModel && error.type.startsWith("billing.err.")) {
          emit({ type: "billing" });
          billing = (await ui.billing(billingModel, error)) ?? undefined;
          if (!billing) { emit({ type: "dismissed" }); return { status: "dismissed", paymentId: billingPaymentID }; }
          continue;
        }
        if (ui && error.type !== "aborted" && await ui.error(error)) continue;
        throw error;
      }
      if (response.kind === "complete") {
        emit({ type: "complete", paymentId: response.payment_id, status: response.status });
        return { status: "complete", paymentId: response.payment_id, paymentStatus: response.status };
      }
      if (response.kind === "billing" && response.billing) {
        if (!ui) throw new CheckoutError("billing_required", "Billing details are required; enable the built-in UI or include them when creating the payment.");
        billingModel = response.billing; billingPaymentID = response.payment_id;
        emit({ type: "billing" }); billing = (await ui.billing(response.billing)) ?? undefined;
        if (!billing) { emit({ type: "dismissed" }); return { status: "dismissed", paymentId: response.payment_id }; }
        continue;
      }
      const action = response.action;
      if (response.kind !== "checkout" || !action) throw new CheckoutError("invalid_response", "Paymux returned an invalid checkout action.");
      ui?.loading("Connecting to the payment provider…");
      if (action.kind === "redirect") {
        const url = safeProviderURL(action.url, "redirect"); emit({ type: "redirecting", url }); ui?.destroy(); win.location.assign(url);
        return { status: "redirecting", paymentId: response.payment_id };
      }
      if (action.kind === "form_post") {
        emit({ type: "redirecting", url: safeProviderURL(action.url, "form") }); ui?.destroy(); submitForm(doc, action);
        return { status: "redirecting", paymentId: response.payment_id };
      }
      if (action.kind === "sdk" && response.launcher) {
        emit({ type: "provider", provider: response.launcher }); ui?.detach();
        let status: "opened" | "dismissed";
        try { status = await openProvider(win, doc, response.launcher, action, options.nonce); }
        catch (raw) {
          const error = checkoutError(raw); emit({ type: "error", error }); ui?.attach();
          if (ui && error.type !== "aborted" && await ui.error(error)) continue;
          throw error;
        }
        if (status === "dismissed") emit({ type: "dismissed" });
        return { status, paymentId: response.payment_id };
      }
      throw new CheckoutError("invalid_action", "Paymux returned an unsupported checkout action.");
    }
  } finally { ui?.destroy(); }
}

/** Execute provider-launch JSON resolved by the merchant backend. */
export async function executeCheckout(response: ActionResponse, options: ExecuteCheckoutOptions = {}): Promise<CheckoutResult> {
	const win = options.window ?? globalThis.window;
	const doc = options.document ?? globalThis.document;
	if (!win || !doc) throw new CheckoutError("browser_required", "Checkout must run in a browser.");
	if (response.kind === "complete") {
		options.onEvent?.({ type: "complete", paymentId: response.payment_id, status: response.status });
		return { status: "complete", paymentId: response.payment_id, paymentStatus: response.status };
	}
	const action = response.action;
	if (response.kind !== "checkout" || !action) throw new CheckoutError("invalid_response", "The merchant backend returned an invalid checkout action.");
	if (action.kind === "redirect") {
		const url = safeProviderURL(action.url, "redirect");
		options.onEvent?.({ type: "redirecting", url });
		win.location.assign(url);
		return { status: "redirecting", paymentId: response.payment_id };
	}
	if (action.kind === "form_post") {
		const url = safeProviderURL(action.url, "form");
		options.onEvent?.({ type: "redirecting", url });
		submitForm(doc, action);
		return { status: "redirecting", paymentId: response.payment_id };
	}
	if (action.kind === "sdk" && response.launcher) {
		options.onEvent?.({ type: "provider", provider: response.launcher });
		const status = await openProvider(win, doc, response.launcher, action, options.nonce);
		if (status === "dismissed") options.onEvent?.({ type: "dismissed" });
		return { status, paymentId: response.payment_id };
	}
	throw new CheckoutError("invalid_action", "The merchant backend returned an unsupported checkout action.");
}

export class PaymuxCheckout {
  constructor(private readonly options: CheckoutOptions) {}
  open(): Promise<CheckoutResult> { return checkout(this.options); }
}
