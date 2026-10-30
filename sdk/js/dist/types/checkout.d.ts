export type CheckoutEvent = {
    type: "loading";
} | {
    type: "billing";
} | {
    type: "provider";
    provider: string;
} | {
    type: "redirecting";
    url: string;
} | {
    type: "dismissed";
} | {
    type: "complete";
    paymentId: string;
    status: string;
} | {
    type: "error";
    error: CheckoutError;
};
export type CheckoutResult = {
    status: "redirecting";
    paymentId: string;
} | {
    status: "opened";
    paymentId: string;
} | {
    status: "dismissed";
    paymentId: string;
} | {
    status: "complete";
    paymentId: string;
    paymentStatus: string;
};
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
export declare class CheckoutError extends Error {
    readonly type: string;
    readonly status: number;
    readonly requestId?: string;
    constructor(type: string, message: string, status?: number, requestId?: string);
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
    states?: Array<{
        code: string;
        name: string;
    }>;
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
export declare function parseCheckoutURL(raw: string, pageURL?: string): ParsedCheckout;
export declare function checkout(options: CheckoutOptions): Promise<CheckoutResult>;
/** Execute provider-launch JSON resolved by the merchant backend. */
export declare function executeCheckout(response: ActionResponse, options?: ExecuteCheckoutOptions): Promise<CheckoutResult>;
export declare class PaymuxCheckout {
    private readonly options;
    constructor(options: CheckoutOptions);
    open(): Promise<CheckoutResult>;
}
export {};
