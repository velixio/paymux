import type * as T from "./types.js";
export type Fetch = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
export interface PaymuxOptions {
    apiKey: string;
    baseURL?: string;
    fetch?: Fetch;
    userAgent?: string;
}
export interface RequestOptions {
    signal?: AbortSignal;
}
export interface MutationOptions extends RequestOptions {
    idempotencyKey: string;
}
export declare class APIError extends Error {
    readonly statusCode: number;
    readonly type: string;
    readonly requestId?: string | undefined;
    readonly idempotentReplay: boolean;
    readonly retryAfterMs: number;
    readonly paymentId?: string | undefined;
    constructor(statusCode: number, type: string, message: string, requestId?: string | undefined, idempotentReplay?: boolean, retryAfterMs?: number, paymentId?: string | undefined);
    get temporary(): boolean;
    get isNotFound(): boolean;
    get isConflict(): boolean;
    static fromResponse(response: Response): Promise<APIError>;
}
export declare class Paymux {
    private readonly apiKey;
    private readonly base;
    private readonly fetcher;
    private readonly userAgent;
    readonly payments: {
        create: (params: T.CreatePaymentParams, options: MutationOptions) => Promise<T.Payment>;
        checkoutAction: (paymentId: string, params: T.CheckoutActionParams, options: MutationOptions) => Promise<T.CheckoutAction>;
        get: (paymentId: string, options?: RequestOptions) => Promise<T.Payment>;
        list: (params?: T.ListPaymentsParams, options?: RequestOptions) => Promise<T.PaymentPage>;
        cancel: (paymentId: string, options: MutationOptions) => Promise<T.Payment>;
    };
    readonly refunds: {
        create: (paymentId: string, params: T.CreateRefundParams, options: MutationOptions) => Promise<T.Refund>;
        get: (refundId: string, options?: RequestOptions) => Promise<T.Refund>;
    };
    readonly paymentLinks: {
        create: (params: T.CreatePaymentLinkParams, options: MutationOptions) => Promise<T.PaymentLink>;
        get: (linkId: string, options?: RequestOptions) => Promise<T.PaymentLink>;
        list: (params?: T.ListPaymentLinksParams, options?: RequestOptions) => Promise<T.PaymentLinkPage>;
        deactivate: (linkId: string, options: MutationOptions) => Promise<T.PaymentLink>;
    };
    readonly subscriptions: {
        create: (params: T.CreateSubscriptionParams, options: MutationOptions) => Promise<T.Subscription>;
        get: (subscriptionId: string, options?: RequestOptions) => Promise<T.Subscription>;
        list: (params?: T.ListSubscriptionsParams, options?: RequestOptions) => Promise<T.SubscriptionPage>;
        cancel: (subscriptionId: string, params: T.CancelSubscriptionParams, options: MutationOptions) => Promise<T.Subscription>;
        update: (subscriptionId: string, params: T.UpdateSubscriptionParams, options: MutationOptions) => Promise<T.Subscription>;
        updatePaymentMethod: (subscriptionId: string, params: T.UpdatePaymentMethodParams, options: MutationOptions) => Promise<T.Subscription>;
    };
    readonly invoices: {
        list: (paymentId: string, options?: RequestOptions) => Promise<T.InvoicePage>;
        get: (invoiceId: string, options?: RequestOptions) => Promise<T.Invoice>;
        downloadPDF: (invoiceId: string, options?: RequestOptions) => Promise<Uint8Array<ArrayBufferLike>>;
    };
    readonly creditNotes: {
        list: (paymentId: string, options?: RequestOptions) => Promise<T.CreditNotePage>;
        get: (creditNoteId: string, options?: RequestOptions) => Promise<T.CreditNote>;
        downloadPDF: (creditNoteId: string, options?: RequestOptions) => Promise<Uint8Array<ArrayBufferLike>>;
    };
    readonly payouts: {
        create: (params: T.CreatePayoutParams, options: MutationOptions) => Promise<T.Payout>;
        get: (payoutId: string, options?: RequestOptions) => Promise<T.Payout>;
        list: (params?: T.ListPayoutsParams, options?: RequestOptions) => Promise<T.PayoutPage>;
    };
    constructor(options: PaymuxOptions);
    private request;
    private requiredList;
    private pdf;
}
