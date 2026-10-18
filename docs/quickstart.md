# Quickstart: a test payment in ten minutes

You will run Paymux on your machine, make a payment, click "Succeed" on a fake
bank page, and watch the signed webhook, the invoice and a refund arrive. No
payment provider account or provider credentials are needed: the **simulator**, a
built-in provider for test mode, plays the bank.

You need a terminal with `curl` and either a release download or Git, Make,
Go (the repository pins its toolchain), and Bun 1.4.3 or newer to build from source. Python 3 is used once, for a ten-line webhook receiver.

## 1. Get the binary (1 minute)

Either build it:

```bash
git clone https://github.com/velixio/paymux && cd paymux
make build                  # writes bin/paymux, bin/paymux-home and the examples
# or: make dev               # builds, then runs the hub and the website together
```

or download `paymux_<version>_<os>_<arch>.tar.gz` from the releases page,
unpack it, and use that `paymux` instead of `bin/paymux` below.

## 2. Start it (1 minute)

Paymux reads its settings from the environment, or from a `.env` file in the
directory you start it in. For a local trial:

```bash
cat > .env <<EOF
PAYMUX_ENV=development
PAYMUX_BASE_URL=http://localhost:8080
PAYMUX_MASTER_KEY=$(head -c32 /dev/urandom | base64)
PAYMUX_DEV_SKIP_2FA=true
PAYMUX_ALLOW_PRIVATE_WEBHOOKS=true
EOF
./bin/paymux
```

What those say: `development` relaxes production-only checks;
`PAYMUX_BASE_URL` is the address customers and the simulator reach Paymux at;
the master key encrypts secrets stored in the database (keep it, you cannot
recover them without it); `PAYMUX_DEV_SKIP_2FA` skips the two-factor step at
sign-in (allowed only in development on a local address); and
`PAYMUX_ALLOW_PRIVATE_WEBHOOKS` lets Paymux deliver webhooks to a receiver on
your own machine. Data goes to `./data` (SQLite) by default.

The log shows a line like this:

```
first-run setup is open: there are no users yet ... url=http://localhost:8080/setup setup_code=abcdef-ghijkl-mnopqr-stuvwx
```

## 3. First-run setup (1 minute)

Open **http://localhost:8080/setup** and enter the setup code from the log. Fill
in your email and a password (12 characters or more), a name for your
organisation, and an app name such as "My Shop".

The next page shows the app's **API keys once**. Copy the **test** key
(`pmx_test_sk_...`) and keep it in a shell variable:

```bash
export KEY=pmx_test_sk_...
```

You are signed in to the dashboard at `/dashboard/`.

## 4. Add the simulator (1 minute)

In the dashboard open **Providers**, choose **Add account**, pick
**Simulator (test mode only)**, name it "Simulator", link it to your app and
save. It needs no configuration. (The simulator can only exist in test mode; it
refuses to be created live.)

You do not need a routing rule: in test mode, payments no rule routes
anywhere go to a linked simulator account.

## 5. Something to receive webhooks (1 minute)

Your app learns about payments from signed webhooks. Save this as
`receiver.py` and run `python3 receiver.py` in another terminal; it prints
every webhook it gets:

```python
from http.server import BaseHTTPRequestHandler, HTTPServer
import json

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        evt = json.loads(body)
        print(evt["type"], json.dumps(evt["data"]["object"])[:160], flush=True)
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass

HTTPServer(("127.0.0.1", 9000), H).serve_forever()
```

In the dashboard open your app, go to **Webhooks**, add the URL
`http://127.0.0.1:9000/` and save. The signing secret (`whsec_...`) is shown
once; a real app uses it to verify each webhook with the
[Standard Webhooks](https://www.standardwebhooks.com) libraries. The receiver
above skips verification and is only for a local trial. For a signed receiver
with a durable inbox, use [the verification example](../examples/webhook-receiver/receiver.py)
and read [the webhook guide](webhooks.md).

## 6. Invoices (optional, 1 minute)

Paymux issues GST invoices for apps that have a tax profile. To see one, open
**Tax entities**, add your business (a legal name, a GSTIN, an address and a
signatory), then open your app's **Tax** tab, pick that entity, tick *Issue
invoices* and save. Skip this step and payments work the same, without
invoices.

## 7. Make a payment (1 minute)

```bash
curl -s localhost:8080/v1/payments \
  -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: quickstart-1" \
  -H "Content-Type: application/json" \
  -d '{"amount": 49900, "currency": "INR", "order_ref": "order-1", "description": "Pro plan",
       "return_url": "https://example.com/thanks",
       "customer": {"external_ref": "u1", "name": "Asha Rao", "email": "asha@example.com",
                    "billing": {"state_code": "29", "country": "IN"}}}'
```

The amount is in minor units (49900 = ₹499.00). The customer's billing state
spares the pay page from asking for it, which GST invoices need. The reply
contains the payment and a `checkout_url`.

Open the `checkout_url` in your browser. It sends you to the simulator's page.
Click **Succeed**. The simulator "captures" the payment and your browser
returns through Paymux to `https://example.com/thanks?...&status=succeeded`
(that site is not yours; the address bar is all you need).

## 8. See what happened (2 minutes)

* **Your receiver** printed `payment.succeeded` (and `payment.created`). The
  payment was confirmed by the simulator's own signed webhook, sent to
  Paymux's `/hooks/...` endpoint exactly like a real provider's, and Paymux
  fetched the payment from the "provider" before believing it.
* **The dashboard**, under **Payments**, shows the payment with its attempt,
  ledger entries, events and webhook deliveries.
* **The API** shows the same, including why a payment failed:

  ```bash
  curl -s localhost:8080/v1/payments/pay_... -H "Authorization: Bearer $KEY"
  ```
* **The invoice** (if you did step 6): under **Invoices** in the dashboard, or
  `curl -s "localhost:8080/v1/invoices?payment_id=pay_..." -H "Authorization: Bearer $KEY"`
  and then `GET /v1/invoices/{id}/pdf` for the PDF.

Now refund part of it:

```bash
curl -s localhost:8080/v1/payments/pay_.../refunds \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"amount": 10000, "reason": "customer request"}'
```

The simulator refunds at once and says so by webhook: your receiver prints
`refund.succeeded`, and a credit note appears next to the invoice.

## 9. Try the awkward cases

Create more payments and use the other buttons on the simulator's page:

| Button | What it shows you |
|---|---|
| **Fail** | A failed attempt (`failure_code` `simulated_failure`); the payment stays payable. |
| **Authorize only** | A payment that is authorised but not captured; the page then offers **Capture**. |
| **Pending → succeed after N seconds** | The customer is back at your site while the payment is still processing; the success arrives later by webhook only. |
| **Late success after expiry** | Success that arrives after you cancelled or the payment expired; Paymux flags it instead of hiding it. |
| **Cancel** | The customer walks away. |

On a succeeded payment's page in the dashboard, **Simulate a dispute** opens a
chargeback: the dispute appears under **Disputes** with a deadline, you can
accept it or submit evidence, and a lost one can get a credit note.

Refunds can be made slow too: on the simulator account's page in the
dashboard set **Refund delay (seconds)** to 5, and refunds stay pending for
five seconds before they succeed.

**Subscriptions.** The simulator supports both billing styles: the provider
bills each period (authorise on the simulator's page, then use **Simulate a
renewal** on the subscription's page in the dashboard, or tick *The charge
fails* to watch it turn past due), and Paymux bills each period against a
UPI-style mandate that the customer authorises on the simulator's page. Create
one with `POST /v1/subscriptions` (see `docs/openapi.yaml`) and see
`docs/connectors.md` for how the simulator behaves.

## Reset and clean up

Stop the server with Ctrl-C. To start over from scratch, delete `./data` (the
database and its lock) and run it again. Test mode data never mixes with live
data, and nothing here reached a real bank.

## Going further

* A real provider: add a Razorpay, PayU, Zoho or Whop account in test or live
  mode (`docs/connectors.md` lists what each needs), then set routing rules
  under **Settings**. For **Zoho Payments**, create the account without a
  refresh token, then press **Connect** on its page: register the redirect URI
  shown there (`<your base URL>/dashboard/oauth/callback`) in a Server-based
  client at `api-console.zoho.in` first (steps in `docs/runbook.md`,
  *Connecting Zoho Payments*).
* Run it for real: set `PAYMUX_ENV=production`, remove `PAYMUX_DEV_SKIP_2FA`,
  use a real `PAYMUX_BASE_URL` behind TLS, and read `docs/runbook.md` and
  `docs/upgrades.md`.
* Extend it: Go and JavaScript hooks, custom connectors, a custom binary:
  `docs/extending.md`.
* The API: `docs/openapi.yaml`.
* This whole flow, scripted: `bash scripts/smoke.sh` builds the binary and
  does what this page describes, against the real thing, in under a minute.
