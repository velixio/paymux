import { useEffect, useRef, useState } from "react";
import {
  ArrowDown,
  ArrowRight,
  ArrowUpRight,
  Check,
  CheckCheck,
  CircleDot,
  Code2,
  Database,
  GitBranch,
  Globe2,
  Layers3,
  LoaderCircle,
  LockKeyhole,
  RotateCcw,
  Server,
  ShieldCheck,
  Terminal,
  Webhook,
  X,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  NativeSelect,
  NativeSelectOption,
} from "@/components/ui/native-select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { CopyButton, SiteButton, SiteLink } from "./components";
import "./homepage.css";

type Scenario = "success" | "declined" | "retry";
type Phase = "idle" | "running" | "done";
const providers = [
  "Razorpay",
  "Stripe",
  "PayU",
  "Whop",
  "Zoho Payments",
  "Cashfree",
];
const request = `curl http://localhost:8080/v1/payments \\\n  -H "Authorization: Bearer $PAYMUX_TEST_KEY" \\\n  -H "Idempotency-Key: order-1048" \\\n  -d '{\n    "amount": 49900,\n    "currency": "INR",\n    "order_ref": "order-1048",\n    "description": "Pro plan"\n  }'`;

function PaymentJourney() {
  const [provider, setProvider] = useState("Razorpay");
  const [scenario, setScenario] = useState<Scenario>("success");
  const [phase, setPhase] = useState<Phase>("idle");
  const [step, setStep] = useState(0);
  const [retried, setRetried] = useState(false);
  const [tab, setTab] = useState("events");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const reset = () => {
    clearTimeout(timer.current);
    setPhase("idle");
    setStep(0);
    setRetried(false);
    setTab("events");
  };
  const run = () => {
    reset();
    setPhase("running");
    const advance = (next: number) => {
      timer.current = setTimeout(() => {
        setStep(next);
        if (next === 4) setPhase("done");
        else advance(next + 1);
      }, 420);
    };
    advance(1);
  };
  const declined = scenario === "declined";
  const waiting = scenario === "retry" && !retried;
  const labels = [
    ["Request accepted", "Idempotency key: order-1048"],
    ["Provider selected", `${provider} · illustrative routing decision`],
    [
      declined ? "Payment declined" : "Payment verified",
      declined
        ? "Sample decline · no captured funds"
        : "Sample capture confirmed with provider",
    ],
    [
      waiting ? "Webhook waiting for retry" : "App webhook delivered",
      waiting
        ? "Sample HTTP 503 · payment remains captured"
        : `Sample HTTP 200 · ${declined ? "payment.failed" : "payment.captured"}`,
    ],
  ];
  return (
    <section
      className="journey-section"
      id="payment-demo"
      aria-labelledby="journey-title"
    >
      <div className="home-section-heading">
        <div>
          <span className="section-note">The work behind a payment</span>
          <h2 id="journey-title">
            Don’t just take our word.
            <br />
            Follow the money.
          </h2>
        </div>
        <p>
          One request touches a lot of systems. See how Paymux connects the
          steps, and what happens when one of them fails.
        </p>
      </div>
      <div className="journey-console">
        <div className="console-bar">
          <span>
            <Terminal aria-hidden="true" size={16} /> Payment workbench
          </span>
          <span className="sample-pill">Interactive demo · sample data</span>
        </div>
        <div className="journey-body">
          <div className="journey-controls">
            <div className="demo-order">
              <span>Example order</span>
              <strong>
                ₹499<span>.00</span>
              </strong>
              <small>Pro plan / order_1048 / INR</small>
            </div>
            <label htmlFor="demo-provider">Payment provider</label>
            <NativeSelect
              id="demo-provider"
              name="demo-provider"
              value={provider}
              disabled={phase === "running"}
              onChange={(e) => {
                reset();
                setProvider(e.target.value);
              }}
            >
              {providers.map((p) => (
                <NativeSelectOption key={p}>{p}</NativeSelectOption>
              ))}
            </NativeSelect>
            <label htmlFor="demo-scenario">What should happen?</label>
            <NativeSelect
              id="demo-scenario"
              name="demo-scenario"
              value={scenario}
              disabled={phase === "running"}
              onChange={(e) => {
                reset();
                setScenario(e.target.value as Scenario);
              }}
            >
              <NativeSelectOption value="success">
                Payment succeeds
              </NativeSelectOption>
              <NativeSelectOption value="declined">
                Provider declines payment
              </NativeSelectOption>
              <NativeSelectOption value="retry">
                Webhook delivery fails
              </NativeSelectOption>
            </NativeSelect>
            <Button
              className="run-payment"
              onClick={run}
              disabled={phase === "running"}
            >
              {phase === "running" ? (
                <LoaderCircle aria-hidden="true" className="spin" size={17} />
              ) : (
                <ArrowRight aria-hidden="true" size={17} />
              )}
              {phase === "running"
                ? "Processing sample…"
                : "Run sample payment"}
            </Button>
            <p className="demo-disclaimer">
              Runs in your browser. No real payment, provider connection, or
              credentials.
            </p>
          </div>
          <div className="journey-output">
            <div className="output-heading">
              <span>Payment activity</span>
              <Button
                variant="ghost"
                size="sm"
                onClick={reset}
                aria-label="Reset payment demo"
              >
                <RotateCcw aria-hidden="true" size={14} /> Reset
              </Button>
            </div>
            <div className="demo-summary" role="status" aria-live="polite">
              <span
                className={`state-dot ${phase === "done" ? (declined ? "is-declined" : "is-complete") : ""}`}
              />
              <strong>
                {phase === "idle"
                  ? "Ready when you are"
                  : phase === "running"
                    ? "Following the payment…"
                    : declined
                      ? "Declined. No capture recorded."
                      : waiting
                        ? "Captured. Delivery needs a retry."
                        : "Captured, recorded, delivered."}
              </strong>
            </div>
            <Tabs value={tab} onValueChange={setTab} className="demo-tabs">
              <TabsList aria-label="Inspect sample payment">
                <TabsTrigger value="events">Event timeline</TabsTrigger>
                <TabsTrigger value="ledger">Ledger</TabsTrigger>
                <TabsTrigger value="webhook">Webhook</TabsTrigger>
              </TabsList>
              <TabsContent value="events">
                <ol className="demo-events">
                  {labels.map(([title, detail], i) => (
                    <li
                      key={i}
                      className={step > i ? "event-complete" : "event-pending"}
                    >
                      <span className="event-symbol">
                        {step > i ? (
                          (i === 2 && declined) || (i === 3 && waiting) ? (
                            <X aria-hidden="true" size={15} />
                          ) : (
                            <Check aria-hidden="true" size={15} />
                          )
                        ) : (
                          <CircleDot aria-hidden="true" size={15} />
                        )}
                      </span>
                      <div>
                        <strong>{title}</strong>
                        <small>
                          {step > i
                            ? detail
                            : phase === "idle"
                              ? "Awaiting sample payment"
                              : "Waiting for previous step"}
                        </small>
                      </div>
                      <span className="event-index">0{i + 1}</span>
                    </li>
                  ))}
                </ol>
              </TabsContent>
              <TabsContent value="ledger">
                <div className="demo-inspector">
                  <Database aria-hidden="true" size={23} />
                  <h3>
                    {step < 3
                      ? "No ledger entries yet"
                      : declined
                        ? "No capture entries"
                        : "Balanced capture entries"}
                  </h3>
                  <p>
                    {step < 3
                      ? "Run a sample to inspect the accounting outcome."
                      : declined
                        ? "A declined payment does not create a captured-funds entry."
                        : "Illustrative double-entry record. Actual accounts depend on your configuration."}
                  </p>
                  {step >= 3 && !declined && (
                    <div className="demo-ledger">
                      <div>
                        <span>Provider receivable</span>
                        <b>Dr ₹499.00</b>
                      </div>
                      <div>
                        <span>Payment clearing</span>
                        <b>Cr ₹499.00</b>
                      </div>
                      <small>Debits = credits · balanced</small>
                    </div>
                  )}
                </div>
              </TabsContent>
              <TabsContent value="webhook">
                <div className="demo-inspector">
                  <Webhook aria-hidden="true" size={23} />
                  <h3>
                    {step < 4
                      ? "No delivery attempt yet"
                      : waiting
                        ? "Delivery failed. Payment didn’t."
                        : "Your app has the event."}
                  </h3>
                  <p>
                    {step < 4
                      ? "The outbound event appears after provider verification."
                      : waiting
                        ? "A delivery failure is tracked separately from payment status. Retry this sample delivery."
                        : "Sample response: HTTP 200. Outbound events are signed for your app to verify."}
                  </p>
                  {step >= 4 && (
                    <>
                      <code className="event-payload">{`{ "type": "${declined ? "payment.failed" : "payment.captured"}",\n  "amount": 49900, "currency": "INR" }`}</code>
                      {waiting && (
                        <Button
                          className="retry-delivery"
                          onClick={() => setRetried(true)}
                        >
                          <RotateCcw aria-hidden="true" size={15} /> Retry
                          sample delivery
                        </Button>
                      )}
                    </>
                  )}
                </div>
              </TabsContent>
            </Tabs>
          </div>
        </div>
        <div className="console-foot">
          <span>
            <LockKeyhole aria-hidden="true" size={13} /> Your infrastructure.
            Your provider accounts.
          </span>
          <SiteLink href="/docs#routing">
            Read the actual routing rules{" "}
            <ArrowUpRight aria-hidden="true" size={14} />
          </SiteLink>
        </div>
      </div>
    </section>
  );
}

export default function Page() {
  return (
    <div className="paymux-home">
      <section className="home-hero">
        <div className="hero-intro">
          <a
            className="open-source-note"
            href="https://github.com/velixio/paymux"
          >
            <span className="small-mark">
              <Code2 aria-hidden="true" size={14} />
            </span>{" "}
            Open source. Self-hosted. Yours.{" "}
            <ArrowUpRight aria-hidden="true" size={13} />
          </a>
          <h1>
            Many providers.
            <br />
            One place to
            <br />
            make sense of it all.
          </h1>
          <p className="home-lead">
            The payment infrastructure between your product and your providers.
            One integration to route payments, trace every event, and stay in
            control.
          </p>
          <div className="home-actions">
            <SiteButton href="/docs" className="home-primary">
              Start building <ArrowUpRight aria-hidden="true" size={17} />
            </SiteButton>
            <a className="home-secondary" href="#payment-demo">
              Take it for a spin <ArrowDown aria-hidden="true" size={16} />
            </a>
          </div>
          <div className="hero-spec">
            <span>
              <Server aria-hidden="true" size={14} /> Single Go binary
            </span>
            <span>
              <Database aria-hidden="true" size={14} /> SQLite or Postgres
            </span>
          </div>
        </div>
        <div
          className="routing-visual"
          role="group"
          aria-label="Architecture: your apps connect to Paymux, which routes to provider accounts and returns verified events"
        >
          <div className="visual-top">
            <span>
              <span className="tiny-dot" /> The payment layer you own
            </span>
            <span>Architecture</span>
          </div>
          <div className="visual-apps">
            <span>
              <Globe2 aria-hidden="true" size={16} /> Storefront
            </span>
            <span>
              <Layers3 aria-hidden="true" size={16} /> SaaS app
            </span>
            <span>
              <Code2 aria-hidden="true" size={16} /> Platform
            </span>
          </div>
          <div className="vertical-wire" />
          <div className="routing-hub">
            <span className="hub-logo">
              <ArrowUpRight aria-hidden="true" size={28} />
            </span>
            <div>
              <strong>paymux</strong>
              <small>One consistent app API</small>
            </div>
            <span className="hub-owned">
              <LockKeyhole aria-hidden="true" size={12} /> Self-hosted
            </span>
          </div>
          <div className="hub-capabilities">
            <span>Route</span>
            <span>Verify</span>
            <span>Record</span>
            <span>Deliver</span>
          </div>
          <div className="routing-wires" aria-hidden="true">
            <span />
            <span />
            <span />
          </div>
          <div className="visual-providers">
            <span>
              <b className="provider-monogram">R</b>Razorpay
            </span>
            <span>
              <b className="provider-monogram">S</b>Stripe
            </span>
            <span>
              <b className="provider-monogram">P</b>PayU
            </span>
          </div>
          <div className="visual-more">+ Whop, Zoho Payments, Cashfree</div>
          <div className="return-event">
            <CheckCheck aria-hidden="true" size={17} />
            <div>
              <strong>One connected record</strong>
              <small>Payment → ledger → signed app event</small>
            </div>
          </div>
          <div className="visual-bottom">
            <span>Provider-specific capabilities apply</span>
            <SiteLink href="/integrations">
              Explore connectors <ArrowUpRight aria-hidden="true" size={13} />
            </SiteLink>
          </div>
        </div>
      </section>
      <div className="home-provider-band">
        <span>
          Keep your provider relationships.
          <br />
          <strong>Bring the integration together.</strong>
        </span>
        <div>
          {providers.map((p) => (
            <span key={p}>{p}</span>
          ))}
        </div>
      </div>
      <section className="home-reason">
        <div className="reason-heading">
          <span className="section-note">Less glue code. More product.</span>
          <h2>
            Your business has enough
            <br />
            moving parts already.
          </h2>
        </div>
        <div className="reason-copy">
          <p>
            Different APIs. Different event formats. Another dashboard when
            something goes wrong.
          </p>
          <p>
            Paymux gives your applications one payment layer, while keeping
            provider differences explicit. Integrate once, then operate with the
            full picture.
          </p>
          <SiteLink href="/integrations">
            See what each connector supports{" "}
            <ArrowRight aria-hidden="true" size={16} />
          </SiteLink>
        </div>
      </section>
      <PaymentJourney />
      <section className="home-operations" id="workspace-preview">
        <div className="home-section-heading">
          <div>
            <span className="section-note">Clarity after checkout</span>
            <h2>
              Payments don’t end
              <br />
              at “successful.”
            </h2>
          </div>
          <p>
            Know where a payment went, what was recorded, and whether your
            application heard about it.
          </p>
        </div>
        <div className="operations-grid">
          <article className="routing-feature">
            <div className="feature-heading">
              <GitBranch aria-hidden="true" size={22} />
              <span>Routing you can explain</span>
            </div>
            <div className="rule-stack">
              <div>
                <span>Application</span>
                <strong>Your storefront</strong>
              </div>
              <div>
                <span>Currency</span>
                <strong>INR</strong>
              </div>
              <div>
                <span>Purpose</span>
                <strong>One-time payment</strong>
              </div>
              <div className="rule-result">
                <Check aria-hidden="true" size={16} />
                <strong>Eligible provider account</strong>
              </div>
            </div>
            <h3>
              Choose the route.
              <br />
              Keep the reasoning.
            </h3>
            <p>
              Ordered rules, currency and purpose matching, and account
              eligibility. Explicit decisions instead of hidden magic.
            </p>
            <SiteLink href="/docs#routing">
              Explore routing <ArrowUpRight aria-hidden="true" size={15} />
            </SiteLink>
          </article>
          <article className="ownership-feature">
            <div className="feature-heading">
              <ShieldCheck aria-hidden="true" size={22} />
              <span>Control that stays with you</span>
            </div>
            <div className="server-illustration">
              <Server aria-hidden="true" size={42} strokeWidth={1} />
              <div>
                <strong>Your infrastructure</strong>
                <span>Paymux + your database</span>
              </div>
              <LockKeyhole aria-hidden="true" size={21} />
            </div>
            <div className="ownership-checks">
              <span>
                <Check aria-hidden="true" size={15} /> Provider credentials on
                your server
              </span>
              <span>
                <Check aria-hidden="true" size={15} /> Inspectable source and
                payment records
              </span>
              <span>
                <Check aria-hidden="true" size={15} /> Your backup and
                deployment policy
              </span>
            </div>
            <h3>
              Your stack.
              <br />
              Your operating rules.
            </h3>
            <p>
              A single Go binary and a database you choose. You own deployment,
              upgrades, secrets, and recovery.
            </p>
            <SiteLink href="/self-hosting">
              Plan your deployment <ArrowUpRight aria-hidden="true" size={15} />
            </SiteLink>
          </article>
        </div>
      </section>
      <section className="home-developer">
        <div>
          <span className="section-note">From curious to first payment</span>
          <h2>
            Start small.
            <br />
            Build something real.
          </h2>
          <p>
            Use Paymux’s built-in simulator before adding a live provider. Try
            success, failure, refunds, and signed webhook delivery with test
            credentials.
          </p>
          <SiteButton href="/docs" className="home-primary">
            Open the quick start <ArrowRight aria-hidden="true" size={16} />
          </SiteButton>
          <span className="developer-footnote">
            No provider account needed to start with the simulator.
          </span>
        </div>
        <div className="home-code">
          <div className="code-heading">
            <span>
              <Terminal aria-hidden="true" size={15} /> Create a payment
            </span>
            <CopyButton target="payment-code" />
          </div>
          <pre>
            <code id="payment-code">{request}</code>
          </pre>
          <div className="code-annotation">
            <span className="tiny-dot" /> One request. Provider-specific work
            handled by connectors.
          </div>
          <small>Amounts use minor units: ₹499.00 = 49900 paise.</small>
        </div>
      </section>
      <section className="home-closing">
        <div className="closing-mark">
          <ArrowUpRight aria-hidden="true" size={32} />
        </div>
        <span className="section-note">Built to be yours</span>
        <h2>
          Own the layer
          <br />
          between you and paid.
        </h2>
        <p>Keep your providers. Take control of the integration.</p>
        <div className="home-actions">
          <SiteButton href="/docs" className="home-primary">
            Build with Paymux <ArrowUpRight aria-hidden="true" size={17} />
          </SiteButton>
          <SiteLink className="home-secondary" href="/integrations">
            Check provider capabilities{" "}
            <ArrowRight aria-hidden="true" size={16} />
          </SiteLink>
        </div>
      </section>
    </div>
  );
}
