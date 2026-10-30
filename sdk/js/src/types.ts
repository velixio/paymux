export type PaymentStatus = "created" | "requires_action" | "processing" | "succeeded" | "failed" | "expired" | "canceled" | "needs_review";
export type SubscriptionStatus = "incomplete" | "incomplete_expired" | "trialing" | "active" | "past_due" | "canceled" | "ended";
export type PayoutStatus = "pending" | "processing" | "succeeded" | "failed" | "reversed" | "needs_review";
export type Interval = "day" | "week" | "month" | "year";
export type PaymentMethod = "upi" | "card" | "emandate";
export type ProrationMode = "none" | "next_invoice" | "immediate";

export interface BillingAddress { legal_name?: string; gstin?: string; line1?: string; line2?: string; city?: string; state_code?: string; postal_code?: string; country?: string }
export interface Customer { external_ref?: string; name?: string; email?: string; phone?: string; billing?: BillingAddress }
export interface CreatePaymentParams { amount: number; currency: string; order_ref?: string; description?: string; customer?: Customer; metadata?: Record<string, string>; return_url?: string; allowed_connectors?: string[]; country?: string }
export interface PaymentAttempt { id: string; status: string; provider: string; provider_payment_id?: string; method?: string; failure_code?: string; failure_message?: string; created_at: string }
export type CheckoutMode = "hosted" | "provider_action";
export interface ProviderAction { kind: "redirect" | "form_post" | "sdk"; url?: string; fields?: Record<string, string>; script_url?: string; options?: Record<string, unknown> }
export type CheckoutAction =
  | { kind: "checkout"; payment_id: string; status: PaymentStatus; launcher: string; action: ProviderAction }
  | { kind: "complete"; payment_id: string; status: PaymentStatus };
export interface CheckoutBilling { name: string; email?: string; country?: string; state_code?: string; line1?: string; postal_code?: string; gstin?: string; legal_name?: string }
export interface CheckoutActionParams { fresh?: boolean; billing?: CheckoutBilling }
export interface Payment { id: string; object: string; livemode: boolean; status: PaymentStatus; kind: string; amount: number; currency: string; amount_received: number; amount_refunded: number; order_ref?: string; description?: string; customer_id?: string; metadata: Record<string, string>; return_url?: string; allowed_connectors?: string[]; country?: string; expires_at: string; created_at: string; version: number; flags: string[]; checkout_mode?: CheckoutMode; checkout_url?: string; checkout?: CheckoutAction; attempts?: PaymentAttempt[] }
export interface ListPaymentsParams extends PageParams { status?: PaymentStatus }
export interface CreateRefundParams { amount?: number; attempt_id?: string; reason?: string }
export interface Refund { id: string; object: string; livemode: boolean; payment_id: string; attempt_id: string; amount: number; currency: string; status: string; reason?: string; initiated_by: string; provider_refund_id?: string; created_at: string }

export interface CreatePaymentLinkParams { amount?: number; min_amount?: number; max_amount?: number; currency: string; description?: string; single_use?: boolean; expires_at?: string; return_url?: string; metadata?: Record<string, string> }
export interface PaymentLink { id: string; object: string; url: string; slug: string; app_id: string; livemode: boolean; amount: number | null; min_amount?: number; max_amount?: number; currency: string; description?: string; return_url?: string; single_use: boolean; active: boolean; expires_at: string | null; metadata: Record<string, string>; deactivated_at?: string; deactivated_reason?: string; created_at: string; version: number }
export interface ListPaymentLinksParams extends PageParams { active?: boolean }

export interface CreateSubscriptionParams { customer: Customer; amount: number; currency: string; interval: Interval; interval_count?: number; trial_days?: number; start_at?: string; total_count?: number; description?: string; metadata?: Record<string, string>; return_url?: string; method?: PaymentMethod; allowed_connectors?: string[]; country?: string }
export interface ProrationOutcome { requested: string; mode: string; amount: number; description?: string; charge_at?: string; reason?: string }
export interface Subscription { id: string; object: string; status: SubscriptionStatus; billing_mode: string; customer_id: string; livemode: boolean; amount: number; currency: string; interval: Interval; interval_count: number; current_period_start: string; current_period_end: string; trial_end?: string; total_count: number; renewals_done: number; cancel_at_period_end: boolean; canceled_at?: string; ended_at?: string; pending_amount?: number; proration_adjust?: number; requires_reauthorization: boolean; description?: string; metadata: Record<string, string>; allowed_connectors?: string[]; country?: string; return_url?: string; mandate_id?: string; created_at: string; version: number; checkout_url?: string; proration?: ProrationOutcome }
export interface ListSubscriptionsParams extends PageParams { customer?: string; status?: SubscriptionStatus }
export interface CancelSubscriptionParams { at_period_end?: boolean }
export interface UpdateSubscriptionParams { amount: number; proration?: ProrationMode }
export interface UpdatePaymentMethodParams { method?: PaymentMethod }

export interface Invoice { id: string; object: string; number: string; payment_id: string; subscription_id?: string; livemode: boolean; issued_at: string; supply_type: string; place_of_supply: string; currency: string; supplier?: unknown; recipient?: unknown; lines?: unknown[]; taxable: number; cgst: number; sgst: number; igst: number; total: number; fx_rate_inr?: string; flags: string[]; template_version: string; pdf_ready: boolean; irn?: string; ack_no?: string; ack_date?: string }
export interface CreditNote { id: string; object: string; number: string; invoice_id: string; payment_id: string; refund_id?: string; dispute_id?: string; livemode: boolean; issued_at: string; reason: string; currency: string; lines?: unknown[]; taxable: number; cgst: number; sgst: number; igst: number; total: number; flags: string[]; template_version: string; pdf_ready: boolean }
export interface Beneficiary { name: string; account_number?: string; ifsc?: string; upi?: string; email?: string; phone?: string }
export interface CreatePayoutParams { amount: number; currency: string; beneficiary: Beneficiary; narration?: string; metadata?: Record<string, string>; provider_account_id?: string; force?: boolean }
export interface Payout { id: string; object: string; status: PayoutStatus; livemode: boolean; amount: number; currency: string; beneficiary: { label: string }; narration?: string; metadata: Record<string, string>; failure_code?: string; failure_message?: string; created_at: string; completed_at?: string; version: number }
export interface ListPayoutsParams extends PageParams { status?: PayoutStatus }

export interface PageParams { limit?: number; starting_after?: string }
export interface Page<T> { object: string; data: T[]; has_more: boolean; next_cursor?: string }
export type PaymentPage = Page<Payment>;
export type PaymentLinkPage = Page<PaymentLink>;
export type SubscriptionPage = Page<Subscription>;
export type PayoutPage = Page<Payout>;
export interface InvoicePage { object: string; data: Invoice[]; has_more: boolean }
export interface CreditNotePage { object: string; data: CreditNote[]; has_more: boolean }
