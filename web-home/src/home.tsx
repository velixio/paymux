import { SiteLink, SiteButton, CopyButton } from "./components";
import { Card } from "@/components/ui/card";
import {
  Table,
  TableHeader,
  TableBody,
  TableRow,
  TableHead,
  TableCell,
} from "@/components/ui/table";
export default function Page() {
  return (
    <>
      <section className="hero">
        <div className="hero-copy">
          <p className="eyebrow">
            <span className="signal" aria-hidden="true"></span>
            {" Open source payment infrastructure"}
          </p>
          <h1>
            {"One integration."}
            <br />
            {"Every payment,"}
            <br />
            {"under your control."}
          </h1>
          <p className="lead">
            {
              "Connect your apps to Paymux. Route payments across providers, keep one ledger, and deliver signed webhooks. All on infrastructure you own."
            }
          </p>
          <div className="actions">
            <SiteButton className="button" href="/docs">
              {"Start self-hosting "}
              <span aria-hidden="true">{"\u2197"}</span>
            </SiteButton>
            <SiteLink className="text-link" href="#architecture">
              {"Explore the architecture"}
            </SiteLink>
          </div>
          <p className="fine">
            {"Single Go binary. SQLite or Postgres. Apache 2.0."}
          </p>
        </div>
        <div className="architecture" id="architecture">
          <div className="diagram-head">
            <span>{"One payment. A connected system."}</span>
            <span className="diagram-tag">{"Architecture"}</span>
          </div>
          <div className="diagram-flow">
            <div className="apps">
              <div>{"Your storefront"}</div>
              <div>{"Your SaaS app"}</div>
              <div>{"Your platform"}</div>
            </div>
            <div className="connector" aria-hidden="true">
              {"\u2192"}
            </div>
            <div className="hub">
              <span className="hub-mark" aria-hidden="true">
                {"\u2197"}
              </span>
              <strong>{"Paymux"}</strong>
              <small>{"Route \u00b7 Verify \u00b7 Record"}</small>
            </div>
            <div className="connector" aria-hidden="true">
              {"\u2192"}
            </div>
            <div className="providers">
              <div>{"Razorpay"}</div>
              <div>{"Stripe"}</div>
              <div>{"PayU India"}</div>
              <div>{"Whop"}</div>
              <div>{"Zoho \u00b7 Cashfree"}</div>
            </div>
          </div>
          <div className="return-path">
            {
              "Provider events \u2192 Verify with provider \u2192 Ledger + outbox \u2192 Signed app webhook"
            }
          </div>
          <div className="diagram-bottom">
            <span>{"Provider-specific connectors"}</span>
            <span>{"One consistent app API"}</span>
          </div>
        </div>
      </section>
      <div className="provider-strip">
        <span>{"Connect to your payment providers"}</span>
        <strong>{"Razorpay"}</strong>
        <strong>{"Stripe"}</strong>
        <strong>{"PayU"}</strong>
        <strong>{"Whop"}</strong>
        <strong>{"Zoho Payments"}</strong>
        <strong>{"Cashfree"}</strong>
      </div>
      <section className="section problem">
        <div>
          <p className="eyebrow">{"Less integration work. More control."}</p>
          <h2>
            {"Your product shouldn\u2019t be"}
            <br />
            {"a collection of payment adapters."}
          </h2>
        </div>
        <p>
          {
            "Every provider brings different APIs, event formats and operational tools. Paymux puts the shared work in one place, while keeping provider capabilities and limitations visible."
          }
        </p>
      </section>
      <section className="pillars">
        <article>
          <span className="feature-icon" aria-hidden="true">
            {"\u21c4"}
          </span>
          <h3>{"Route with intent"}</h3>
          <p>
            {
              "Choose providers by app, currency and purpose. Ordered rules and account eligibility keep routing decisions explicit."
            }
          </p>
          <SiteLink href="/docs#routing">{"How routing works"}</SiteLink>
        </article>
        <article>
          <span className="feature-icon" aria-hidden="true">
            {"\u2261"}
          </span>
          <h3>{"Follow the money"}</h3>
          <p>
            {
              "Payment state, ledger records and outbound events commit together. Inspect attempts, refunds and delivery history in one dashboard."
            }
          </p>
          <SiteLink href="#workspace-preview">{"See the workflow"}</SiteLink>
        </article>
        <article>
          <span className="feature-icon" aria-hidden="true">
            {"\u2318"}
          </span>
          <h3>{"Own the whole system"}</h3>
          <p>
            {
              "Deploy a single binary, choose SQLite or Postgres, and extend connectors through Go packages or a signed HTTP service."
            }
          </p>
          <SiteLink href="/self-hosting">{"Plan your deployment"}</SiteLink>
        </article>
      </section>
      <section className="section workspace-section" id="workspace-preview">
        <div className="section-heading">
          <div>
            <p className="eyebrow">{"Built for the work after checkout"}</p>
            <h2>
              {"A payment record,"}
              <br />
              {"with the full picture."}
            </h2>
          </div>
          <p>
            {
              "Provider confirmation, ledger entries and webhook delivery are different events. Your interface should make each one clear."
            }
          </p>
        </div>
        <Card className="product-preview">
          <div className="preview-sidebar">
            <b>{"paymux"}</b>
            <span className="selected">{"Payments"}</span>
            <span>{"Subscriptions"}</span>
            <span>{"Reconciliation"}</span>
            <span>{"Invoices"}</span>
            <span>{"Providers"}</span>
          </div>
          <div className="preview-main">
            <div className="preview-title">
              <strong>{"Payment details"}</strong>
              <span className="sample">{"Illustrative sample"}</span>
            </div>
            <div className="preview-amount">
              {"\u20b9499.00 "}
              <span className="success">{"Succeeded"}</span>
            </div>
            <dl>
              <dt>{"Provider"}</dt>
              <dd>{"Razorpay"}</dd>
              <dt>{"Order reference"}</dt>
              <dd>{"order_1048"}</dd>
              <dt>{"Currency"}</dt>
              <dd>{"INR"}</dd>
            </dl>
            <div className="event-row">
              <span className="event-dot"></span>
              <div>
                <strong>{"Provider confirmed"}</strong>
                <small>{"Payment verified with the provider"}</small>
              </div>
            </div>
            <div className="event-row">
              <span className="event-dot"></span>
              <div>
                <strong>{"Ledger recorded"}</strong>
                <small>{"Committed with the payment event"}</small>
              </div>
            </div>
            <div className="event-row">
              <span className="event-dot"></span>
              <div>
                <strong>{"App webhook delivered"}</strong>
                <small>{"Separate delivery status \u00b7 HTTP 200"}</small>
              </div>
            </div>
          </div>
        </Card>
      </section>
      <section className="section developer">
        <div>
          <p className="eyebrow">{"Start with a test payment"}</p>
          <h2>
            {"Your first integration"}
            <br />
            {"doesn\u2019t need a bank account."}
          </h2>
          <p>
            {
              "Use the built-in simulator to try payment success, failure, refunds and signed webhook delivery before adding a live provider."
            }
          </p>
          <SiteButton className="button" href="/docs">
            {"Open the quick start"}
          </SiteButton>
        </div>
        <div className="code-panel">
          <div>
            <span>{"Create a payment"}</span>
            <CopyButton target="payment-code" />
          </div>
          <pre>
            <code id="payment-code">
              {
                'curl http://localhost:8080/v1/payments \\\n  -H "Authorization: Bearer $PAYMUX_TEST_KEY" \\\n  -H "Idempotency-Key: order-1048" \\\n  -d \'{\n    "amount": 49900,\n    "currency": "INR",\n    "order_ref": "order-1048",\n    "description": "Pro plan"\n  }\''
              }
            </code>
          </pre>
          <p>
            {"Amounts use minor currency units. \u20b9499.00 = 49900 paise."}
          </p>
        </div>
      </section>
      <section className="section closing">
        <p className="eyebrow">{"Make payment infrastructure yours"}</p>
        <h2>
          {"Keep your providers."}
          <br />
          {"Take control of the integration."}
        </h2>
        <div className="actions">
          <SiteButton className="button" href="/docs">
            {"Get started with Paymux"}
          </SiteButton>
          <SiteLink className="text-link" href="/integrations">
            {"Check provider capabilities"}
          </SiteLink>
        </div>
      </section>
    </>
  );
}
