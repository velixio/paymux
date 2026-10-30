import { APIError, Paymux, WebhookVerifier, type Payment } from "@paymux-js/sdk";
import { CheckoutError, checkout, executeCheckout, type ActionResponse } from "@paymux-js/sdk/checkout";

const client = new Paymux({ apiKey: "test", baseURL: "http://localhost:8080" });
const payment: Promise<Payment> = client.payments.get("pay_1");
const created = client.payments.create({ amount: 100, currency: "INR" }, { idempotencyKey: "order-1" });
const action = client.payments.checkoutAction("pay_1", {}, { idempotencyKey: "order-1-checkout" });
const verifier = new WebhookVerifier(`whsec_${btoa("test-key")}`);

void payment;
void verifier;
void action.then((value) => executeCheckout(value));
void created.then((value) => value.checkout && executeCheckout(value.checkout));
void APIError;
void CheckoutError;
void checkout;
void executeCheckout;
void (null as unknown as ActionResponse);
