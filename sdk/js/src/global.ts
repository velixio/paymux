import { CheckoutError, PaymuxCheckout, checkout, parseCheckoutURL } from "./checkout";

const api = { checkout, PaymuxCheckout, CheckoutError, parseCheckoutURL };
Object.assign(globalThis, { Paymux: api });
export default api;
