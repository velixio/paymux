import { CheckoutError, PaymuxCheckout, checkout, executeCheckout, parseCheckoutURL } from "./checkout.js";

const api = { checkout, executeCheckout, PaymuxCheckout, CheckoutError, parseCheckoutURL };
Object.assign(globalThis, { Paymux: api });
export default api;
