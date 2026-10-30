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
interface ParsedCheckout {
    endpoint: string;
    token: string;
}
export declare function parseCheckoutURL(raw: string, pageURL?: string): ParsedCheckout;
export declare function checkout(options: CheckoutOptions): Promise<CheckoutResult>;
export declare class PaymuxCheckout {
    private readonly options;
    constructor(options: CheckoutOptions);
    open(): Promise<CheckoutResult>;
}
export {};
