# Paymux runbook

How to install, run, back up and recover a Paymux hub. Commands assume Linux with systemd; a Docker variant is given where it differs. Everything is one static binary, `paymux`, plus a database (SQLite by default, Postgres optional) and a reverse proxy for TLS.

Contents: [Layout](#layout) · [Install](#install) · [First admin and 2FA](#first-admin-and-2fa) · [Apps and provider accounts](#apps-and-provider-accounts) · [Payment links](#payment-links) · [GST e-invoice](#gst-e-invoice-offline-mode) · [Alerts](#alerts) · [Themes and languages](#themes-and-languages) · [Webhook URLs](#webhook-urls-to-register-at-each-provider) · [Configuration reference](#configuration-reference) · [Backups and restore](#backups-and-restore) · [Upgrades](#upgrades) · [Monitoring](#monitoring) · [If the hub is down](#if-the-hub-is-down)

## Layout

| Piece | Where | Notes |
|---|---|---|
| Hub | `paymux serve` | Listens on `PAYMUX_LISTEN` (default `:8080`) for everything public, and on `PAYMUX_INTERNAL_LISTEN` (default `127.0.0.1:9100`) for `/metrics`, `/debug/pprof` and Caddy's `/internal/tls-allow`. Never expose the internal listener to the internet. |
| Data directory | `PAYMUX_DATA_DIR` (default `./data`; `/var/lib/paymux` in the unit) | Holds `paymux.db` (SQLite), its lock file, the optional `themes/<name>/` themes and the optional `custom/` overrides. |
| Reverse proxy | Caddy (`deploy/Caddyfile`) | TLS for the hub's host and, on demand, for app custom domains. |
| Config | `/etc/paymux/paymux.env` | `KEY=value` lines, read by systemd. A `.env` file in the working directory is also read for CLI use. |
| Master key | `PAYMUX_MASTER_KEY` or `PAYMUX_MASTER_KEY_FILE` | 32 random bytes, base64. Encrypts provider credentials, webhook secrets and TOTP secrets. **Back it up separately from the database; without it the encrypted data cannot be read.** |

## Install

### Binary and systemd

```sh
# 1. The binary (build with `make build`, or download a release) and a service user.
install -m 0755 bin/paymux /usr/local/bin/paymux
useradd --system --home /var/lib/paymux --shell /usr/sbin/nologin paymux
install -d -o paymux -g paymux -m 0700 /var/lib/paymux
install -d -m 0750 -g paymux /etc/paymux

# 2. A master key, kept in a file only root and the service can read.
umask 077
head -c 32 /dev/urandom | base64 > /etc/paymux/master.key
chown root:paymux /etc/paymux/master.key && chmod 0640 /etc/paymux/master.key
#    Store a copy in your password manager / vault NOW.

# 3. The environment file.
cat > /etc/paymux/paymux.env <<'EOF'
PAYMUX_ENV=production
PAYMUX_BASE_URL=https://pay.example.com
PAYMUX_DATA_DIR=/var/lib/paymux
PAYMUX_MASTER_KEY_FILE=/etc/paymux/master.key
PAYMUX_TZ=Asia/Kolkata
# Behind Caddy on the same machine:
# PAYMUX_TRUSTED_PROXIES defaults to the local network, which covers Caddy on 127.0.0.1
# Where alerts go (optional but strongly recommended):
PAYMUX_OPS_EMAILS=ops@example.com
PAYMUX_OPS_WEBHOOK_URL=https://hooks.slack.com/services/XXX/YYY/ZZZ
# Outgoing mail (customer invoices, alerts). Leave PAYMUX_SMTP_HOST unset to disable email.
PAYMUX_SMTP_HOST=smtp.example.com
PAYMUX_SMTP_PORT=587
PAYMUX_SMTP_USERNAME=paymux
PAYMUX_SMTP_PASSWORD=change-me
PAYMUX_SMTP_FROM=Paymux <billing@example.com>
EOF
chown root:paymux /etc/paymux/paymux.env && chmod 0640 /etc/paymux/paymux.env

# 4. Start the service. The schema is created on first start; the first admin is
#    created in the browser (see "First admin and 2FA").
cp deploy/paymux.service /etc/systemd/system/ && systemctl daemon-reload
systemctl enable --now paymux
journalctl -u paymux -f            # structured JSON logs; look for "first-run setup is open"
curl -fsS http://127.0.0.1:8080/readyz
```

`PAYMUX_BASE_URL` must be the public https URL (plain `http://` is accepted only for localhost). In `production` the hub refuses to start without a valid master key and refuses `PAYMUX_DEV_SKIP_2FA`.

### Caddy

Install Caddy, copy `deploy/Caddyfile` to `/etc/caddy/Caddyfile`, replace `pay.example.com` and the e-mail address, and `systemctl reload caddy`. Point DNS for the hub's host at this server. App custom domains (added on the app's *Domains* tab) get certificates on demand, but only for hosts the hub knows: Caddy asks `http://127.0.0.1:9100/internal/tls-allow?domain=...` before it requests one. The hub says yes for its own host, for a domain whose ownership was verified (active), and for a domain still pending only while its A or AAAA record already points at the hub's address (the same address `PAYMUX_BASE_URL`'s host resolves to). Anything else gets no certificate, so adding names in the dashboard cannot use up the CA's issuance limits.

**nginx on another device instead of Caddy:** [`deploy/nginx/paymux.shimul.me.conf`](../deploy/nginx/paymux.shimul.me.conf) proxies `paymux.shimul.me` to the Paymux host on the LAN (`192.168.0.174:8050`). `PAYMUX_TRUSTED_PROXIES` defaults to the local network, so the nginx device is believed without a setting; set its exact address for a stricter setup. Allow port 8050 only from that device. nginx has no on-demand TLS, so each app custom domain needs its own server block and certificate.

### Docker

```sh
docker build -f deploy/docker/Dockerfile -t paymux:latest --build-arg VERSION=$(git describe --always) .   # or: make docker
docker run -d --name paymux --restart unless-stopped \
  -p 127.0.0.1:8080:8080 -v paymux-data:/data \
  --env-file /etc/paymux/paymux.env -e PAYMUX_DATA_DIR=/data \
  -e PAYMUX_MASTER_KEY="$(cat /etc/paymux/master.key)" \
  paymux:latest
```

The image is distroless, runs as user 65532 and contains only the binary. There is no shell: run CLI commands with `docker run --rm -v paymux-data:/data --env-file ... paymux:latest user reset-2fa ...` (stop the container first when using SQLite, see below). The first admin is created in the browser, so the CLI is not needed to get started; the setup code is in `docker logs paymux`. The internal listener stays on the container's loopback, so Caddy must run in the same network namespace (`--network container:paymux`, or on the host with `--network host`), or set `PAYMUX_INTERNAL_LISTEN=0.0.0.0:9100` on a private Docker network that only Caddy joins.

### Postgres instead of SQLite

Set `PAYMUX_DATABASE_URL=postgres://user:password@host:5432/paymux?sslmode=verify-full`. `verify-full` checks the server's certificate and host name; `require` encrypts but accepts any certificate, so a party on the path can impersonate the database. For a private CA add `&sslrootcert=/path/ca.pem`. Paymux logs a warning at startup when the host is not on this machine and `sslmode` is missing, `disable`, `allow`, `prefer` or `require`. Migrations run at start. Use Postgres when you want more than one hub process, managed backups or point-in-time recovery. The hub logs the URL with the password masked.

## First admin and 2FA

There is no default account. A hub that has no users shows a one-time setup page, so nothing has to be run on the server and the SQLite file never has to be unlocked:

1. Start the hub and read its log. It prints the address and a one-time **setup code** (24 letters in four groups):

   ```
   journalctl -u paymux | grep "first-run setup"      # or: docker logs paymux | grep "first-run setup"
   ```

2. Open `https://pay.example.com/setup` (the login page links to it while the hub has no users), enter the code, and fill in the administrator's email and password (at least 12 characters), the organisation name and, optionally, a first app.
3. The next page shows, **once**, a test and a live API key for the app (copy them now), and the key for two-factor sign-in: add it to an authenticator app (Google Authenticator, Authy, 1Password and similar) by hand or through the `otpauth://` link, then enter the 6-digit code. That signs you in.

How it is protected:

- The code exists only in the hub's log and memory, and is new at every start while the hub has no users. Five wrong codes from one address lock the page for 15 minutes.
- The moment the administrator exists, `/setup` answers 404 for good, and the code is gone. Anyone who can reach the hub before you cannot use the page without the code, but they could race you if the log is public: do the setup straight after the first start, and keep the log private.
- The setup is audited (`setup.complete`); the password, code, API keys and 2FA key are never logged or audited.
- If the page is closed before the 2FA code is confirmed, sign in at `/dashboard/login` with the password: the same key is shown again until the first code is accepted. Lost the API keys: create new ones on the app's page.

The command line still works for scripted installs and for recovery. **With SQLite the CLI cannot run while `paymux serve` is running**: the database file is locked by one process, so stop the service for the moment it takes. (With Postgres there is no such limit.)

```sh
paymux user create --email you@example.com --role admin                      # prompts for the password twice
printf '%s\n' "$PASSWORD" | paymux user create --email you@example.com --role admin --password-stdin
```

Codes cannot be reused, and five wrong sign-in attempts per email and address lock sign-in for 15 minutes.

- Lost authenticator: `paymux user reset-2fa --email you@example.com` (signs the user out everywhere and prints a one-time enrolment code; they enter it at the next sign-in to set up a new authenticator). A new user gets such a code when an admin creates them (Users page or `paymux user create`); the Users page can issue another. The code works once, for 7 days, and knowing only the password is not enough to enrol.
- More users: *Users* in the dashboard (admins create users with an initial password; roles are viewer, operator, admin).
- `PAYMUX_DEV_SKIP_2FA=true` exists for local development only and is refused in production or on a non-local base URL.

## Apps and provider accounts

Everything below is in the dashboard (admin role); the same can be scripted with `paymux app create`, `paymux provider add`, `paymux tax-entity add`, `paymux app tax`, `paymux domain add` and `paymux fx set`.

1. **Tax entity** (*Tax entities*): the legal entity and GSTIN that issues invoices, with its invoice prefix, signatory, logo and LUT. Skip this if no app issues invoices.
2. **App** (*Apps → Create an app*): name, slug and a default return URL. On the app page:
   - *Keys*: create a **test** and a **live** API key. The token is shown once; store it in the app's secret store. Revoke keys here too.
   - *Webhooks*: add the app's endpoint URL; copy the signing secret (shown once, `whsec_...`, Standard Webhooks). Test with *Deliveries* and *Replay*.
   - *Branding*: display name, accent colour, logo (PNG/JPEG up to 512 KB).
   - *Tax*: which tax entity, SAC, GST rate, prices inclusive or not, issue invoices on or off.
   - *Settings*: per-app overrides of the org defaults.
3. **Provider accounts** (*Providers → Add an account*; for Zoho Payments see [Connecting Zoho Payments](#connecting-zoho-payments)): pick the connector, fill the credentials the form asks for (secrets are write-only afterwards), tick the currencies and purposes, and link the apps that may use it. Use *Test connection* to check the credentials. Create separate **test** and **live** accounts.
4. **Provider access** (app → *Providers* tab) lists which accounts the app may use; **routing** (*Settings → Routing*) decides which of them takes a payment, and *Test routing* shows the outcome for a sample payment.
5. **Custom domain** (app → *Domains*), optional: add `pay.yourapp.com`, point its DNS (CNAME/A) at this server, and register the domain with each provider (see the checklist on that tab; Razorpay and PayU review merchant websites, so do this before going live).
6. **Settings** (*Settings*): payment expiry, invoice delivery, dunning, retry schedule, reconciliation time and the rest, each with its default and units.

Before going live, run a payment end to end in **test** mode (the header toggle switches the dashboard between test and live data), then check *Reconciliation* after the first day.

## Connecting Zoho Payments

Zoho Payments needs an OAuth client (client ID and secret) and a **refresh token** for it. You do not copy tokens by hand: create the account, then press **Connect**.

1. **Create the account** (*Providers → Add an account → Zoho Payments*): Zoho account id, client ID, client secret, webhook signing key and the widget API key (or hosted checkout URL). Leave *OAuth refresh token* empty. The account is created *Not connected*; until it is connected, using it fails with "not connected" (nothing is sent to Zoho).
2. **Create a Server-based client** in Zoho's API console (`api-console.zoho.in`, or your data centre's: `.com`, `.eu`, `.com.au`, `.jp`, `.sa`, `.ca`). A *Self Client* cannot use the redirect flow (see below).
3. **Register the redirect URI.** The account page shows it, exactly as Zoho must have it: `<PAYMUX_BASE_URL>/dashboard/oauth/callback` (for example `https://pay.example.com/dashboard/oauth/callback`), with a Copy button. Add it to the client's *Authorized redirect URIs*. If Zoho answers *Invalid Redirect Uri*, the URI registered there differs from the one shown (scheme, host, port, path or a trailing slash): fix the client, not Paymux. `PAYMUX_BASE_URL` must be the address you open the dashboard on.
4. **Connect.** On the account page, under *Connect with Zoho Payments*, enter the client ID and secret (a blank secret keeps the stored one), check the scopes (the defaults are the ones Paymux uses, with the `ZohoPaySandbox` prefix for a sandbox account) and press **Connect**. You go to Zoho, approve, and return to the account page, which now says *Connected* and lists the granted scopes. Then press **Test connection**.
5. **Reconnect** the same way when the token is revoked, the scopes change or you rotate the client secret.

Good to know:

- Only an **admin** can connect. The authorization is valid for ten minutes and for one use, and only for the user who started it; an expired, repeated or foreign callback is refused with nothing stored.
- Zoho tells the callback which data centre the account lives in; Paymux follows only Zoho's own accounts hosts for that and records the data centre in the account.
- **Self Client fallback.** If your client is a Self Client, generate a grant code in the API console (choose the same scopes, a few minutes' validity) and paste it into *Or paste a grant code* on the same panel, with the client ID and secret. It is exchanged without a redirect and the refresh token is stored the same way.
- The client secret and tokens are sealed in the account's config; they are never shown again, logged or written to the audit log (which records only that the account was connected, by whom, and with which scopes).

## Payment links

A payment link is a page you can send to a customer (`https://pay.example.com/l/<slug>`, or on the app's custom domain) without writing any code: they enter their name, email and optionally phone, and Paymux creates a normal payment and takes them to the usual payment page, where the billing details the GST rules ask for are collected as always. Create links in the dashboard under *Payment links* (operators and admins), or with `POST /v1/payment_links`.

- **Price.** A fixed amount, or "the customer chooses" between a minimum and an optional maximum.
- **Reusable or single-use.** A reusable link takes any number of payments. A single-use link is *held* by the first visitor while their payment is open (60 minutes by default, the `payment_ttl` setting): a second visitor is asked to try again later, and the first visitor can come back with the same browser. The first **successful** payment switches the link off (`payment_link.deactivated`, reason `used`). A payment that is abandoned frees the link when its time is up.
- **Expiry and switching off.** An expired or deactivated link shows a notice and creates nothing. Deactivating is permanent and does not affect payments already started.
- **Your records.** Every payment from a link carries `metadata.payment_link_id` (plus the link's own metadata) and `order_ref` set to the link id, so webhooks, the dashboard and exports can tell where it came from. Events: `payment_link.created`, `payment_link.deactivated`.
- **Limits.** Anyone can open a link, so each link allows at most 60 new payments a minute (a "please wait" page after that) and the form is protected against cross-site posting. The page uses the app's theme and `customer_language`.

The API reference for these and all other endpoints is served by the hub itself at `https://pay.example.com/docs` (the spec: `/docs/openapi.yaml`).

## GST e-invoice (offline mode)

Businesses above the e-invoicing turnover threshold must register their B2B and export invoices with the government's Invoice Registration Portal (IRP) and print the IRN and a QR code on them. (The threshold has been lowered several times; check the current rule for your turnover. The tax entity's *turnover band* is only a note.) Paymux does not talk to the IRP; it prepares the file and records the answer, so you can do it by hand:

1. **Download.** Open a B2B or export invoice in the dashboard and press *Download e-invoice JSON*, or on *GST reports* choose the tax entity and month and press *Export month as e-invoice JSON* (a JSON array, for the portal's bulk upload; consumer invoices are skipped and invoices that already have an IRN are left out unless you ask for them). The files are in the portal's INV-01 version 1.1 format and are checked against the committed schema `internal/invoice/einvoice/schema/einvoice-1.1.json` and the portal's arithmetic before you get them.
2. **Upload** the file in the IRP's offline tool or bulk upload page. The portal returns, per invoice, the IRN (64 characters), the acknowledgement number and date and the signed QR code.
3. **Record** them on the invoice's page under *e-Invoice*: IRN, Ack No, Ack Date (as the portal prints it, IST) and the signed QR code text. Paymux refuses a QR code that is not the portal's token for *this* invoice (it must name the same IRN, document number and seller GSTIN) and an IRN that is already on another invoice. The invoice PDF is rendered again with the IRN, the acknowledgement and the QR code; the customer's hosted page shows them too; the API invoice carries `irn`, `ack_no` and `ack_date`. A wrong entry can be corrected by recording again; every change is audited.

When the file cannot be built, the page says why, in plain words, for example: the seller's address must end with a 6-digit PIN code ("1 Marine Drive, Mumbai 400020": fix it on the tax entity), the buyer needs a GSTIN, an address line, a city and a PIN, the document number may be at most 16 characters (shorten the invoice prefix), or the FX rate of an export is not set yet.

What to know:

- Invoices issued before this feature keep their address as one line of text; Paymux takes the PIN from it and the last comma-separated part as the locality, which is right for ordinary Indian addresses. New invoices keep the address in parts.
- Exports under LUT are sent as `EXPWOP`, exports with IGST as `EXPWP`, buyer `URP`, place of supply 96, PIN 999999, the foreign currency and country in `ExpDtls`; all rupee values use the same FX rate as the PDF. `RefClm` (claiming a refund of the IGST paid) is `N` for an export under LUT, which paid none, and for an export that paid IGST it follows the org setting `einvoice_igst_refund_claim`: `auto` (the default) sends `Y`, the usual claim of an exporter who pays IGST; `Y` and `N` force it. Decide with whoever files your returns; the setting only changes the file you upload.
- **Credit notes** of B2B and export invoices have their own file, a `CRN` document that names the invoice it answers (`RefDtls.PrecDocDtls`) and carries the credit note's own number, date and amounts (positive, as the portal wants them). Open the credit note in the dashboard and press *Download e-invoice JSON*. Credit notes of consumer invoices have none. Upload it the same way; the IRN of a credit note is not recorded in Paymux.
- The schema was written from the portal's published field list and is **not verified against the portal's own schema file**, and the signature of the signed QR code is not verified; both are listed in [provider verification](provider-verification.md). Upload one invoice first and watch the portal's validation messages.

## GST amounts, buyers abroad and the tax settings

- **Prices with or without tax.** An app's tax profile says whether its prices include GST. With *prices include tax* (the default) the amount of a payment is what the customer pays and the tax is worked out inside it. Without it, the **amount you send is the taxable value**: Paymux charges the taxable value plus GST at the profile's rate (nothing added for an export under a valid LUT), the payment's `amount` in the response is that total, and the invoice total always equals `amount_received`. This applies to payments, payment links and subscription renewals alike, and only when the app issues invoices.
- **Subscriptions ask too.** The hosted subscription page (`/s/{id}`) shows the same billing form as the pay page, following the same `billing_details` setting, *before* the buyer is sent to the provider (nothing is created at the provider until the form is answered, and a mandate for a tax-exclusive price is sized after it). The answers are stored on the customer, and every invoice of the subscription, renewals included, uses them. The form is skipped when the customer already has the details, which is how an app that sends `customer.billing` with the subscription avoids it. Do send it when you can: a subscription without a known State or country is invoiced as a sale in the supplier's own State and flagged `pos_defaulted`.
- **When the country is not known.** A payment or subscription in rupees is taken to be an Indian buyer's. In any other currency, a buyer whose country is unknown is asked for it, as a required field (when the app issues invoices and `billing_details` is not `off`). If it is still unknown (the setting is `off`, or invoicing was added later) the invoice is a domestic sale in the supplier's State with the `pos_defaulted` flag and an ops alert, never a zero-rated export: treating a buyer as a foreigner is a claim that needs evidence, charging GST is the safe error, and a credit note puts it right. A buyer who says they are in India while paying in dollars is asked for the State.
- **Buyer country.** The pay page asks for the buyer's country (two letters, `IN` by default) along with the State. A buyer outside India needs no State or PIN, and the sale is an export: IGST, or zero-rated under a valid LUT when paid in a foreign currency. A payment in rupees from a buyer abroad is an export with IGST, and is flagged `export_igst`. The API takes the country as `customer.billing.country`.
- **Ops alerts.** The tax exceptions the monthly report lists (`pos_defaulted`, `export_igst`, `fx_pending`, `credit_note_cutoff_passed`) also raise an ops alert when they happen, once per document, with the ids needed to find it (kinds `tax.pos_defaulted` and so on; see *Alerts*).
- **Settings** (Settings page, org level): `credit_note_cutoff` is the last day, as `MM-DD`, after the end of the financial year of the sale on which a credit note can still be issued (`11-30`, the legal day, by default; `off` never stops one), and `b2cl_threshold` is the invoice value in paise above which an inter-state sale to a consumer goes on the B2CL sheet (`10000000`, Rs 1,00,000, by default).
- **E-invoice settings** (same page): `einvoice_igst_refund_claim` (`auto`, `Y` or `N`, see above) and `einvoice_irp_keys`, a list of the portal's PEM certificates or RSA public keys. With a key set, the signed QR code you record must carry a valid RS256 signature by it (the audit log notes whether the signature was verified); with none, only what the token says is checked. Paymux ships no key: take it from the portal's documentation.
- **Verification debit.** A mandate registration with nothing due debits Rs 1 to verify it. Where it is not certain that the provider reverses it, `verification_debit_fallback` (`refund_after_72h`, the default, or `never`) makes Paymux look at the payment at the provider after 72 hours and refund the debit itself if it has not been returned. It never refunds a debit that was refunded by anyone, and a repeat is harmless.
- **Provider minimum charges.** Each connector states the smallest amount its provider will charge (for example Rs 1 for Razorpay, 50 cents for Stripe in USD). A plan-change credit never takes a renewal below it: what does not fit waits for the renewals after, and a small extra charge for an upgrade is added to the next renewal instead of being charged on its own.

## Digital signature certificate (DSC)

By default an invoice PDF carries the authorised signatory's name and the line "This is a computer-generated document". If you have a digital signature certificate (class 2 or 3, from a certifying authority), upload it on the tax entity's page: Paymux then signs every PDF of that entity (invoices and credit notes) with a PAdES signature (a detached CAdES signature added to the end of the file, so the document itself is not altered), and the page reads "Digitally signed by ...".

- **The file** is the PKCS#12 (`.p12` or `.pfx`) the authority gives you, with its password. Both are stored encrypted and are never shown again. Paymux reads the older 3DES/RC2 encryption most authorities issue. A file made by OpenSSL 3 uses AES by default and is refused with a message; export it again with `openssl pkcs12 -export -legacy -certpbe PBE-SHA1-3DES -keypbe PBE-SHA1-3DES -macalg sha1 ...`. RSA and ECDSA keys are supported. The certificate must be valid when it is uploaded.
- **Which documents.** Documents rendered after the upload; an earlier PDF stays as it was. Removing the certificate (on the same page) goes back to the signatory note.
- **If it stops working** (the certificate expires, the file cannot be opened), invoices still go out, with the signatory note, and an ops alert `tax.dsc_unusable` tells you. The page warns 30 days before expiry.
- **What is not done.** No trusted timestamp (PAdES-T) and no revocation data (PAdES-LT) are added, and the signature has been checked with OpenSSL and an independent parser in the tests, not in Adobe Acrobat or against the Indian CCA trust list: open a signed test invoice in a PDF reader you trust before relying on it.

## Alerts

*Alerts* in the dashboard lists everything Paymux raised for the operator: provider accounts that went silent, failed reconciliations, dispute deadlines, failing custom domains, failing hooks and unusable themes. Each alert is still sent by email and webhook as before (`PAYMUX_OPS_EMAILS`, `PAYMUX_OPS_WEBHOOK_URL`); the page keeps it, so nothing is lost with a mail that went to spam.

- The nav shows the number of open alerts, in red when one is critical.
- A condition that happens again while its alert is open (same kind and same ids) adds to the alert's count and updates its message instead of adding a row; the email and webhook keep their own 10-minute rate limit.
- Operators and admins press *Acknowledge* once somebody has looked; it is audited (`alert.acknowledge`). If the condition returns later it opens a new alert. Filter by open, acknowledged or all, severity and kind.
- Alerts are kept indefinitely; there is no pruning yet.

## Payouts in needs_review

A payout whose submission to the provider kept ending in a timeout or a cut connection (10 attempts) goes to `needs_review`: the provider may or may not have paid it. Paymux stops sending, polls the provider for it by its reference (a provider answer moves it on by itself), and raises a critical alert `payout.needs_review`.

- **Check the provider's dashboard** (or bank statement) for the payout's reference, shown in the alert.
- **If it did not happen or did not pay,** and you want to send it again, send a new payout with `"force": true` (the API refuses a payout to the same beneficiary and amount while one is in review; the force is audited as `payout.force_create`). Resolve the old one as failed first, or it may be paid twice if the provider acts late.
- **To settle it by hand** (the provider will not answer for it): `paymux payout resolve --id po_... --status succeeded|failed --reason "what you checked"`. It works only on a payout in `needs_review`, sends the matching `payout.succeeded` or `payout.failed` event once, and is audited (`payout.resolve`, with the reason). Like the other commands that open the database, it needs `paymux serve` stopped on SQLite.

## Live accounts never run with sandbox settings

A live provider account whose stored config is a sandbox one (a sandbox or test switch, a test key such as `sk_test_` or `rzp_test_`, PayU's public test keys, Cashfree test credentials, Zoho sandbox scopes) is refused, and so is a test account with live keys. At start-up the hub disables such accounts (audit entry `provider_account.disable_sandbox_on_live`) and raises a critical alert `provider.sandbox_on_live`; the connector is also never built while the config stays as it is, so enabling the account again does not help. Edit the account with the provider's live credentials. The hub keeps running without the account. Stripe events whose `livemode` is not the environment of the account's key are refused.

## Logs

*Logs* in the dashboard (menu: Operations) is where to look when something failed: it keeps the **errors and warnings** of the hub, so you can review and fix them later. Info and debug lines still go only to stdout and journald. It needs the `logs.read` permission: admins have it, and a custom role can be given it under *Roles*.

**What is kept**

- *Errors* (kept 90 days): application errors, failed provider calls, 5xx responses and jobs that failed for good. A failed provider call row shows the request we sent and the answer, with the provider's request id, how long it took, and the payment, attempt, refund, subscription, mandate or payout it was for. That is usually enough to see why a PayU call timed out or a Zoho call answered 400.
- *Warnings* (kept 7 days): warnings, retries, and 4xx answers on the API and webhook routes (a bad signature, a rejected API key, a validation error). Warnings carry ids only, no bodies.
- A call that worked leaves no row, and neither does ordinary page traffic.
- **Secrets and buyers' details are removed before a row is written** (keys, tokens, passwords, signatures, cards, names, e-mail addresses, phones, addresses, UPI handles, GSTIN, PAN). A row names the payment or refund it belongs to; look the details up on that payment's page. See [the security review](security-review.md).

**Finding things.** Filter by time, level, kind (`app`, `provider`, `http`, `job`), connector, provider account, request id, payment id, any other id (attempt, refund, subscription, mandate, payout, job, the provider's own order or payment id, a webhook event id) and free text (it searches the message, the ids and the bodies). Every id on a row is a link to the rows that share it. A customer or an API caller who quotes a *request id* (it is in the `request_id` of an API error, in the `X-Request-Id` header, and at the end of a plain error page) leads straight to the rows of that request. The payment, attempt, refund and subscription pages have a *Logs for this ...* link for users who may read the logs. *Export JSON lines* downloads the filtered rows (up to 10,000, bodies included, still redacted), for pasting into a bug report.

**Retention and size.** Settings (*Settings*, Delivery and retention): `log_retention_error_days` (default 90), `log_retention_warn_days` (default 7) and `log_max_rows` (default 500,000) and `log_max_mb` (default 1024; the space the rows' text takes). When either is exceeded the oldest rows go first, checked as rows arrive and not only by the hourly job, which also purges by age in batches. Rejected API and webhook requests (4xx) are rate limited so that a flood cannot fill the log: one row per source, route and status a minute, at most 30 a minute per route and status from all sources together, and the next row carries a `suppressed` count of what was skipped (also exported as the `paymux_logs_suppressed_total` metric). `audit_retention_days` (default 400, 0 = forever, never below 365) does the same for the audit log.

**Where it lives.** With SQLite the rows are in their own file, `<data dir>/logs.db`, so writing them never competes with payments for the database lock. `paymux backup` does not include it. It is always safe to delete (stop Paymux, remove the file, start Paymux); Paymux makes a new one and sets a damaged one aside as `logs.db.corrupt-<time>`. With PostgreSQL it is the `logs` table of the main database; add `--exclude-table-data=logs` to `pg_dump` to leave the rows out of a dump.

**File modes.** The databases hold sealed secrets and customer data. Paymux creates them (and the `-wal` and `-shm` files SQLite adds) readable by its own user only, and the Docker image creates `/data` with mode 0700 for user 65532. A file that already existed keeps the mode it had: if one is readable by group or others, Paymux logs a warning at startup that names the file and `chmod 600`. It does not change the mode itself, because a backup agent may rely on it.

**Settings of the log itself**

| Variable | Default | Meaning |
|---|---|---|
| `PAYMUX_DB_LOG_LEVEL` | `warn` | The lowest level of application records kept (`warn`, `error`; `info` and `debug` only when you ask for them by name, they are plentiful). Failed provider calls and failing requests are kept whatever it is. |
| `PAYMUX_OTLP_LOGS_ENDPOINT` | unset | When set, the same redacted rows are also posted to an OpenTelemetry collector over OTLP/HTTP (JSON), for example `http://collector:4318/v1/logs`. The address may be private. Unset means off. |
| `PAYMUX_OTLP_LOGS_HEADERS` | unset | `Name=value,Name=value` sent with every OTLP request (an auth token). Treated as a secret. |

Rows are written in batches by a queue. If the queue is ever full (a very slow disk), rows are dropped and counted: `paymux_logs_dropped_total` on `/metrics`, and a line "log rows dropped" on stdout every minute. `paymux_logs_stored_total`, `paymux_logs_write_failures_total` and `paymux_logs_queue_depth` complete the picture.

## Themes and languages

**Themes** change how the dashboard and the customer pages (pay page, subscription checkout, invoices) look. Two ship with the hub: `default` and `minimal-dark`. Choose them in the dashboard under *Appearance* (admins): one theme for the dashboard (also the default for customer pages) and, per app, an optional theme and language for that app's customer pages. *Preview* shows a sample page in any theme before you switch.

A theme is a directory `<data dir>/themes/<name>/`:

```
themes/acme/
  theme.json        {"name":"acme","version":"1.0.0","description":"...",
                     "tokens":{"--color-accent":"#0a7d5a","--radius":"4px"},
                     "font_stack":"Inter, system-ui, sans-serif"}
  css/*.css         optional extra stylesheets, applied in name order after the tokens
  templates/*.html  optional template overrides (same file names as web/templates)
  assets/...        images and fonts, served at /theme/acme/assets/<file>
```

- `name` must equal the directory name (lower-case letters, digits, hyphens). Tokens are CSS custom properties (`--color-bg`, `--color-fg`, `--color-muted`, `--color-accent`, `--color-danger`, `--color-line`, `--color-nav`, `--radius`, ...); a value may not contain `;`, braces, angle brackets, `url(` or comments. Look at `web/themes/` in the source for the two bundled examples.
- Overlay order is **embedded files, then the active theme, then `custom/`**: a file in `custom/templates/` beats the same file in the theme.
- A new or edited theme is live within a second: no restart. A theme that is invalid (bad `theme.json`, a template that does not parse, missing directory) is skipped, its pages use the standard look, and ops get a `theme.unusable` alert (logged, e-mailed and posted like other alerts, once per problem). *Appearance* lists such themes with the reason.
- A theme in the data directory with the same name as a bundled one replaces it.

**Languages.** Customer pages and the pre-debit e-mail are available in English (`en`) and Hindi (`hi`). Set the language per app under *Appearance* (the `customer_language` setting; the org value is the default). A link may also carry `?lang=hi` to choose an installed language for that visit; anything else is ignored. A message that is missing in a language shows in English.

To change wording or add a language, put `i18n/<code>.json` in the `custom/` directory (flat `"key": "text"` pairs; copy `web/i18n/en.json` for the keys, and keep `%s` placeholders). Entries are merged over the shipped catalog of the same language, so a file with one key rewords just that message. The code is two or three lower-case letters. Catalogs are read at start: restart after editing them. The pre-debit e-mail text itself is `templates/email_predebit.txt` (override it in `custom/templates/`).

## Webhook URLs to register at each provider

Each provider account has its own URL: `https://pay.example.com/hooks/<provider_account_id>`. It is shown, with a copy button, on the account's detail page. Register it in the provider's dashboard and subscribe to payment, refund and dispute events. Paymux treats a webhook only as a hint and re-fetches the object from the provider's API, so a missing event delays things rather than corrupting them; reconciliation catches the rest.

| Provider | What to do | Notes |
|---|---|---|
| **Razorpay** | Dashboard → Account & Settings → Webhooks → add the URL, set a secret, tick payment, refund and dispute events (subscription events when you use subscriptions). Put the same secret in the account's *webhook secret* field. | Signature header `X-Razorpay-Signature`. Razorpay disables a webhook that keeps failing for about 24 hours: re-enable it after any outage. Up to 30 URLs per account. |
| **PayU** | Configure the success/failure/refund/dispute notification URLs in the PayU merchant dashboard to the account's URL. | Verified by PayU's reverse hash. **Refund webhooks are unsigned**: Paymux never trusts them and only fetches the refund from PayU's API. |
| **Zoho Payments** | Add the URL and signing key in the Zoho Payments developer settings. | At most 5 webhook endpoints per account. Sandbox must be enabled by Zoho support. Zoho has no dispute API: enter disputes by hand (*Disputes → Record a dispute by hand*). Zoho disables failing webhooks after about 2 days. |
| **Whop** | Add the URL in the Whop developer dashboard for your app. | Standard Webhooks signatures; the connector pins `api_version: v1`. Whop disables failing webhooks after about 72 hours. Whop is merchant of record, so Paymux issues no invoice for it. |

Provider menus change; the descriptions above name the settings to look for, not exact clicks. A *silence detector* raises an alert when an account created payment attempts recently but no verified webhook has arrived.

## Configuration reference

| Variable | Default | Meaning |
|---|---|---|
| `PAYMUX_ENV` | `production` | `production` or `development`. |
| `PAYMUX_BASE_URL` | required | Public origin, `https://...`. |
| `PAYMUX_LISTEN` | `:8080` | Public listener (behind Caddy). |
| `PAYMUX_INTERNAL_LISTEN` | `127.0.0.1:9100` | Metrics, pprof, TLS-ask. Keep it private. |
| `PAYMUX_DATA_DIR` | `./data` | SQLite file, lock, `custom/` theme. |
| `PAYMUX_DATABASE_URL` | `sqlite://<data dir>/paymux.db` | `sqlite://path` or `postgres://...`. |
| `PAYMUX_MASTER_KEY` / `PAYMUX_MASTER_KEY_FILE` | exactly one required | Base64, 32 bytes. |
| `PAYMUX_TZ` | `Asia/Kolkata` | Time zone of "today", reconciliation time and date filters. |
| `PAYMUX_TRUSTED_PROXIES` | local network | Comma-separated CIDRs/addresses of reverse proxies whose `X-Forwarded-For` is believed (for the sign-in limit, sessions and audit). The default is the local network (loopback, private and link-local ranges); `none` believes no proxy. A host on a trusted network can set the client address Paymux records, so name the proxy's exact address where the LAN is shared. A `/0` block is refused. |
| `PAYMUX_CUSTOMER_RATE` | `60` | Requests a minute per client address and customer route family (`/pay/`, `/return/`, `/l/`, `/s/`, `/i/`), burst 20; over it the answer is 429 with `Retry-After`. `0` disables. IPv6 clients are limited per /64 (every address in one /64 shares a bucket). Behind a proxy, `PAYMUX_TRUSTED_PROXIES` must name it, or all customers share the proxy's bucket. |
| `PAYMUX_ALLOW_PRIVATE_WEBHOOKS` | `false` | Allow outgoing webhooks to private/loopback addresses (development). |
| `PAYMUX_SMTP_HOST`, `_PORT` (587), `_USERNAME`, `_PASSWORD`, `_FROM` | unset | Outgoing mail over STARTTLS. Mail and credentials are never sent in clear text. |
| `PAYMUX_OPS_EMAILS`, `PAYMUX_OPS_WEBHOOK_URL` | unset | Where ops alerts go (email and a Slack-compatible webhook). Identical alerts are sent at most every 10 minutes. |
| `PAYMUX_DB_LOG_LEVEL` | `warn` | Lowest level of application records kept on the Logs page (see *Logs*). |
| `PAYMUX_OTLP_LOGS_ENDPOINT`, `PAYMUX_OTLP_LOGS_HEADERS` | unset | Also ship the stored log rows to an OpenTelemetry collector (OTLP/HTTP JSON). |
| `PAYMUX_ENV_FILE` | `.env` | Extra env file for CLI use. |

## Dashboard sessions

How long a sign-in lasts is three settings (*Settings*, *Sign-in sessions*): `session_idle_hours` (12), `session_max_days` (7) and `session_remember_days` (30; 0 removes the *Remember me* checkbox). To end sessions: a user's own list is under **Your sessions**; for someone else use **Sign out everywhere** on the Users page (also done by disabling the user, a password change, `paymux user reset-2fa` and a new enrolment code). If a laptop or a cookie is lost, sign the user out everywhere and change their password. Setting `session_remember_days` to `0` stops new remembered sign-ins at once and makes existing ones follow the ordinary limits.

## Rotating the master key

Rotate the key when it may have leaked, when a person who had it leaves, or on your own schedule. The master key encrypts provider credentials, webhook signing secrets, authenticator secrets, payout beneficiaries, signing certificates, connector state and stored API responses; the key rotation re-encrypts all of them. Nothing is lost if you follow the order below, and the hub never has to stop for longer than a restart.

1. **Back up the database** and the current key (`paymux backup --to ...`, or `pg_dump`).
2. **Make the new key**: `head -c 32 /dev/urandom | base64`. Keep it somewhere other than the database's backups.
3. **Switch**: set `PAYMUX_MASTER_KEY` (or the file) to the new key and `PAYMUX_MASTER_KEY_OLD` (or `PAYMUX_MASTER_KEY_OLD_FILE`) to the old one. Several old keys can be listed, comma separated, if you rotated before and not every value was resealed. Restart the hub. It seals new values under the new key and still opens the old ones. Everyone is signed out once (sessions and form tokens are derived from the key).
4. **Reseal**: `paymux secrets reseal --dry-run` shows how many values would change; `paymux secrets reseal` rewrites them all under the new key in one transaction and prints the count. It refuses, and changes nothing, if any value cannot be opened (a key is missing from the old list, a row is damaged): it names the table and row, never the value. It is safe to run again, and a second run reports `resealed 0`. On SQLite stop the hub first (the data directory is locked); on PostgreSQL it can run beside the hub, and a value that changes while it runs makes it stop with no change, so run it again.
5. **Remove the old key** from `PAYMUX_MASTER_KEY_OLD` once the command reports nothing under an old key, and restart. Destroy the old key and any backup that only it can open when your retention allows. A leaked old key still opens old backups.

If the old key is lost before step 4, the values sealed under it cannot be read and have to be entered again (provider credentials, webhook secrets) or reset (authenticators: `paymux user reset-2fa`).

## Backups and restore

Back up **three things**: the database, the master key, and `paymux.env`. Test a restore before you need one.

### SQLite with Litestream (default)

Litestream streams every change to S3-compatible storage, typically within a second.

```sh
# install litestream, then:
cp deploy/litestream.yml /etc/litestream.yml       # edit bucket, endpoint, path
install -m 0600 /dev/null /etc/litestream.env      # LITESTREAM_ACCESS_KEY_ID / LITESTREAM_SECRET_ACCESS_KEY
systemctl enable --now litestream
litestream snapshots -config /etc/litestream.yml /var/lib/paymux/paymux.db   # should list snapshots
```

**Restore** (new machine or damaged file), with the hub stopped:

```sh
systemctl stop paymux litestream
mv /var/lib/paymux/paymux.db /var/lib/paymux/paymux.db.broken 2>/dev/null
litestream restore -config /etc/litestream.yml -o /var/lib/paymux/paymux.db /var/lib/paymux/paymux.db
# a point in time:  litestream restore -timestamp 2026-11-03T09:00:00Z ...
chown paymux:paymux /var/lib/paymux/paymux.db
systemctl start litestream paymux
curl -fsS http://127.0.0.1:8080/readyz
```

Also keep an occasional offline copy: `sqlite3 /var/lib/paymux/paymux.db ".backup '/backups/paymux-$(date +%F).db'"` is safe while the hub runs.

### Postgres

- **Logical backups**: `pg_dump -Fc -d paymux -f paymux-$(date +%F).dump` nightly, copied off the machine.
- **Point-in-time recovery**: enable WAL archiving (`archive_mode=on`, an `archive_command` to object storage, or use pgBackRest / WAL-G / your cloud's managed PITR) and take a base backup daily.
- **Restore a dump**: stop the hub, `createdb paymux_restore && pg_restore -d paymux_restore paymux.dump`, point `PAYMUX_DATABASE_URL` at it, start the hub.
- **Restore to a point in time**: restore the base backup, set `recovery_target_time`, start Postgres, then start the hub.

### After any restore

1. Start the hub and check `/readyz`.
2. The restore point is behind real life. In *Reconciliation* press **Run now** for every provider account: it re-reads payments and refunds from the providers and repairs or lists the differences.
3. Check *Disputes* for new ones and re-enable any provider webhook that was switched off while the hub was away.
4. In each app's *Webhooks* tab, replay failed deliveries (*Deliveries → Replay*). Apps drop snapshots older than the one they have, so replays are safe.

## Upgrades

1. Read the release notes for migrations or setting changes.
2. Take a backup (an extra `litestream` snapshot, `sqlite3 .backup`, or `pg_dump`).
3. Get the new binary and check it. A release has one archive per platform (`paymux_<version>_<os>_<arch>.tar.gz`, linux and darwin, amd64 and arm64), a `SHA256SUMS` file and, when it was signed, `SHA256SUMS.sig` and `SHA256SUMS.pem`:

   ```sh
   sha256sum -c --ignore-missing SHA256SUMS
   cosign verify-blob --certificate SHA256SUMS.pem --signature SHA256SUMS.sig \
     --certificate-identity-regexp '^https://github.com/OWNER/REPO/' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
   tar -xzf paymux_v1.2.3_linux_amd64.tar.gz paymux && ./paymux version
   ```

   (Building from source: `scripts/release.sh v1.2.3` makes the same archives locally.)
4. Replace the binary (or `docker pull`/rebuild) and restart: `systemctl restart paymux`. The hub applies new migrations when it starts and reports `/readyz` as not ready while any are pending. You can run `paymux migrate` first to apply them without serving.
5. Check `/readyz`, `journalctl -u paymux -n 50` and a test-mode payment.
6. **Rollback** means restoring the backup of step 2 together with the old binary; migrations only move forward.

In-flight jobs finish on shutdown (up to 30 seconds); queued jobs survive a restart.

## Monitoring

- `GET /healthz` is liveness; `GET /readyz` checks the database, pending migrations, the job queue and the scheduler. Probe `/readyz` from outside too.
- Prometheus metrics are on the internal listener: `curl http://127.0.0.1:9100/metrics` (request counts, job queue depth by state, oldest pending job age, last heartbeat).
- Alerts (`PAYMUX_OPS_EMAILS`, `PAYMUX_OPS_WEBHOOK_URL`): failed reconciliation, dispute deadlines (on open, 72 h and 24 h before, on loss), webhook silence, endpoints failing for a day.
- Audit log: *Audit* in the dashboard (admins) lists every operator action with the actor and address.
- Errors: *Logs* in the dashboard (see *Logs*) keeps the errors, failed provider calls and rejected requests. Watch `paymux_logs_dropped_total` and `paymux_logs_write_failures_total`: both should stay at 0.

## If the hub is down

**What stops:** customers cannot open pay pages or complete new payments through the hub; apps' API calls fail. **What does not:** money already paid is safe at the provider, and providers keep retrying their webhooks for a while (Razorpay about 24 h before it disables the endpoint, Zoho about 2 days, Whop 72 h). Apps keep what they have; they should retry API calls with the same `Idempotency-Key`.

1. **Is it the process?** `systemctl status paymux`, `journalctl -u paymux -n 100`. Common causes: a missing or wrong master key (it refuses to start rather than guess), a full disk, another process holding the SQLite lock (`database is locked by another paymux process`: a CLI command or a second instance is running), Postgres unreachable, an unapplied migration after an upgrade.
2. **Restart it**: `systemctl restart paymux`; confirm `/readyz`. Caddy returns 502 until the hub answers.
3. **Is the data bad?** Restore from backup (above) onto the same or a new machine; DNS or Caddy must point at it.
4. **Hub is up but behind a bad deploy?** Roll back the binary and, if migrations ran, the backup taken before the upgrade.
5. **After recovery, catch up:**
   - *Reconciliation → Run now* for each provider account (this finds payments that were paid at the provider while the hub could not hear about it).
   - Look at *Disputes* for deadlines that passed.
   - Re-enable any provider webhook that was disabled for failures; send a test event.
   - Replay failed outbound webhooks for each app (*App → Webhooks → Deliveries*), and ask apps to reconcile open orders with `GET /v1/payments/{id}` if they missed `payment.succeeded`.
6. **Tell customers** with open orders that the payment status will update shortly; their provider receipt is proof of payment in the meantime.

If the master key is lost, the database cannot be decrypted: provider credentials, webhook secrets and TOTP secrets must be entered again (new provider accounts, new endpoint secrets, `reset-2fa` for every user). This is why the key is backed up separately.
