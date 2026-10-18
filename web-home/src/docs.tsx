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
      <section className="page-intro">
        <p className="eyebrow">{"Developer quick start"}</p>
        <h1>
          {"From source code"}
          <br />
          {"to a test payment."}
        </h1>
        <p className="lead">
          {
            "Run Paymux locally, create your first app and try the simulator. Add a real provider when you\u2019re ready."
          }
        </p>
      </section>
      <section className="section doc-layout">
        <nav className="doc-nav" aria-label="On this page">
          <SiteLink href="#install">{"Build and run"}</SiteLink>
          <SiteLink href="#setup">{"Create your workspace"}</SiteLink>
          <SiteLink href="#routing">{"Configure routing"}</SiteLink>
          <SiteLink href="#webhooks">{"Verify delivery"}</SiteLink>
          <SiteLink href="#reference">{"API and SDK"}</SiteLink>
        </nav>
        <div className="doc-body">
          <section id="install">
            <h2>{"Build and run"}</h2>
            <p>
              {
                "The repository pins its Go toolchain. Use the build target to produce the payment hub binary."
              }
            </p>
            <div className="code-panel">
              <div>
                <span>{"Terminal"}</span>
                <CopyButton target="install-code" />
              </div>
              <pre>
                <code id="install-code">
                  {
                    "git clone https://github.com/velixio/paymux.git\ncd paymux\nmake build\ncp .env.example .env"
                  }
                </code>
              </pre>
            </div>
            <p>
              {"Set "}
              <code>{"PAYMUX_MASTER_KEY"}</code>
              {" to a base64-encoded random 32-byte key and "}
              <code>{"PAYMUX_BASE_URL"}</code>
              {
                " to your hub\u2019s address. Keep the master key backed up securely."
              }
            </p>
            <div className="code-panel">
              <pre>
                <code>{"./bin/paymux"}</code>
              </pre>
            </div>
          </section>
          <section id="setup">
            <h2>{"Create your workspace"}</h2>
            <p>
              {"Open "}
              <code>{"/setup"}</code>
              {
                " on your running hub. Enter the one-time code printed in the log, create the administrator, configure 2FA and create an app. The app\u2019s API keys are shown once."
              }
            </p>
            <p>
              {
                "Switch to test mode and add the Simulator provider. It needs no provider credentials. The "
              }
              <SiteLink href="https://github.com/velixio/paymux/blob/main/docs/quickstart.md">
                {"complete simulator walkthrough"}
              </SiteLink>
              {
                " includes the local development configuration, webhook receiver and first payment."
              }
            </p>
          </section>
          <section id="routing">
            <h2>{"Configure routing"}</h2>
            <p>
              {
                "Paymux evaluates app rules before organization rules and the catch-all. Accounts must be eligible for the requested mode, currency and purpose. The dashboard\u2019s route tester shows which rule and accounts match."
              }
            </p>
            <p>
              {
                "Keep test and live accounts distinct. Provider capabilities determine whether refunds, subscriptions, mandates or dispute responses are available."
              }
            </p>
          </section>
          <section id="webhooks">
            <h2>{"Verify delivery"}</h2>
            <p>
              {
                "Your app receives signed events in the Standard Webhooks format. Verify signatures before processing events, handle repeated delivery idempotently, and use the payment API to inspect the authoritative state. A browser redirect alone is not payment confirmation."
              }
            </p>
            <SiteLink href="https://github.com/velixio/paymux/tree/main/examples">
              {"Explore integration examples"}
            </SiteLink>
          </section>
          <section id="reference">
            <h2>{"API and SDK"}</h2>
            <p>
              {"The running payment hub serves its API reference at "}
              <code>{"/docs"}</code>
              {" and its specification at "}
              <code>{"/docs/openapi.yaml"}</code>
              {
                ". This public website is a separate binary and does not expose your hub."
              }
            </p>
            <div className="resource-links">
              <SiteLink href="https://github.com/velixio/paymux/blob/main/docs/openapi.yaml">
                {"OpenAPI specification"}
              </SiteLink>
              <SiteLink href="https://github.com/velixio/paymux/tree/main/pkg">
                {"Public Go packages"}
              </SiteLink>
              <SiteLink href="https://github.com/velixio/paymux/blob/main/docs/extending.md">
                {"Hooks and framework mode"}
              </SiteLink>
              <SiteLink href="/self-hosting">
                {"Deployment responsibilities"}
              </SiteLink>
            </div>
          </section>
        </div>
      </section>
    </>
  );
}
