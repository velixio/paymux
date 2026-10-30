import { CheckoutError, PaymuxCheckout, checkout, parseCheckoutURL } from "./checkout.js";

const api = { checkout, PaymuxCheckout, CheckoutError, parseCheckoutURL };
Object.assign(globalThis, { Paymux: api });
export default api;
