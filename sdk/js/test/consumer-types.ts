import { APIError, Paymux, WebhookVerifier, type Payment } from "@paymux-js/sdk";
import { CheckoutError, checkout } from "@paymux-js/sdk/checkout";

const client = new Paymux({ apiKey: "test", baseURL: "http://localhost:8080" });
const payment: Promise<Payment> = client.payments.get("pay_1");
const verifier = new WebhookVerifier(`whsec_${btoa("test-key")}`);

void payment;
void verifier;
void APIError;
void CheckoutError;
void checkout;
