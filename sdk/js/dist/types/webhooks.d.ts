export type WebhookHeaders = Headers | Record<string, string | string[] | undefined>;
export interface WebhookEvent<T = unknown> {
    id: string;
    type: string;
    timestamp: string;
    livemode: boolean;
    data: {
        object: T;
    };
}
export interface WebhookVerifierOptions {
    toleranceMs?: number;
    now?: () => number;
}
export declare class WebhookError extends Error {
    readonly type: "headers" | "timestamp" | "signature" | "payload";
    constructor(type: "headers" | "timestamp" | "signature" | "payload", message: string);
}
/** Verifies Standard Webhooks signatures against the exact raw request bytes. */
export declare class WebhookVerifier {
    private readonly key;
    private readonly toleranceMs;
    private readonly now;
    constructor(secret: string, options?: WebhookVerifierOptions);
    verify<T = unknown>(headers: WebhookHeaders, rawBody: string | Uint8Array): Promise<WebhookEvent<T>>;
}
