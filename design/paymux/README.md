# Paymux dashboard design

Open `dashboard.html` in a browser. It is a standalone, dependency-free interactive design proposal; it does not modify the production dashboard.

## Direction

A light operational workspace with a white 222px sidebar, cool neutral canvas, restrained blue interaction color, and semantic payment statuses. Payment records carry the hierarchy; summaries are secondary. Use Segoe UI with system fallbacks, tabular currency figures, 44px controls, 8px spacing increments, 7px control corners and 10px panel corners. Core colors: ink #182337, muted #5e6a7d, border #e1e6ed, surface #ffffff, canvas #f6f8fb, blue #2256d9. Success, pending and failure always include text.

The automated design-system search initially returned a marketing pattern; that pattern was rejected. A focused financial-dashboard style lookup supported dense financial records, currency formatting and audit context. The final layout is tailored to Paymux rather than adopting the generated landing-page structure.

## Working prototype

- Search ID, order, customer name or email.
- Combine status tabs and provider filter; reset filters.
- Export the filtered sample records as CSV.
- Open a payment detail dialog, close with Close or Escape.
- Responsive layout: compact horizontal navigation on mobile; horizontally scrollable data table.
- Filtered totals are INR-only and derived from the fictional rows. No currency aggregation or real account data.

## Reference evidence and limits

The supplied authenticated Razorpay URL redirected to login; the Whop business URL was inaccessible; the PayU URL did not expose readable application content. No private dashboard data or exact current visual styling was inspected.

Public first-party references used for workflow patterns:

- Razorpay payment details, settlement context, refunds and webhook actions: https://razorpay.com/docs/payments/payments/dashboard/
- PayU transaction overview, filtering, export and details: https://docs.payu.in/docs/transactions
- PayU transaction columns and date ranges: https://docs.payu.in/docs/track-transactions

The prototype is an original Paymux interpretation. Whop-specific styling remains unverified.

## Implementation boundary

Production integration should retain server-rendered Go templates, existing filter parameters, authorization, CSRF and test/live separation. Current production payment states, available actions and provider capabilities remain authoritative. Provider filtering, summary aggregations and export controls in this prototype require backend support before being added to production. Do not infer webhook delivery from payment success. Actual ledger entries, webhook attempts and provider confirmation must be queried independently.

Next design expansion: overview, payment detail with real ledger rows and webhook attempt history, then providers and reconciliation. Financial mutations need their own permission-aware confirmation designs; this prototype performs none.

## Chromium verification

Verified in headless Chromium at 1440px, 768px, 390px and 360px. Search, combined status/provider filtering, empty state, reset, detail dialog, Escape dismissal and CSV download passed. No JavaScript page errors or page-level horizontal overflow were detected. Screenshot review led to simplifying mobile navigation and stacking summary labels, amounts and captions. Saved screenshots: `desktop.png`, `payment-detail.png`, and `viewport-{768,390,360}.png`.
