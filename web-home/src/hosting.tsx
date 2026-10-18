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
        <p className="eyebrow">{"Self-hosting and security"}</p>
        <h1>
          {"Your infrastructure."}
          <br />
          {"Your operational control."}
        </h1>
        <p className="lead">
          {
            "Run Paymux as a single Go binary or container. You choose the database, deployment environment and backup strategy."
          }
        </p>
      </section>
      <section className="section hosting-grid">
        <article>
          <h2>{"A deliberate deployment"}</h2>
          <p>
            {
              "Use SQLite for a single-instance installation or Postgres for your database infrastructure. A hardened systemd service, distroless Docker image and reverse-proxy examples are included."
            }
          </p>
          <SiteLink href="https://github.com/velixio/paymux/tree/main/deploy">
            {"Explore the deployment kit"}
          </SiteLink>
        </article>
        <article>
          <h2>{"Secrets stay on your side"}</h2>
          <p>
            {
              "The master key encrypts stored provider secrets. Terminate TLS, restrict administrative access, configure trusted proxies and retain a secure copy of the encryption key."
            }
          </p>
          <SiteLink href="https://github.com/velixio/paymux/blob/main/docs/runbook.md">
            {"Read the operations runbook"}
          </SiteLink>
        </article>
        <article>
          <h2>{"Backups are part of the system"}</h2>
          <p>
            {
              "Use Litestream for SQLite or your Postgres backup strategy. Back up the database and required configuration, test restoration and back up before an upgrade."
            }
          </p>
          <SiteLink href="https://github.com/velixio/paymux/blob/main/docs/upgrades.md">
            {"Plan upgrades and recovery"}
          </SiteLink>
        </article>
        <article>
          <h2>{"Operate with evidence"}</h2>
          <p>
            {
              "Health endpoints, structured logs, Prometheus metrics, audit records and operational alerts help you investigate. Configure monitoring, delivery endpoints and incident ownership."
            }
          </p>
          <SiteLink href="https://github.com/velixio/paymux/blob/main/docs/security-review.md">
            {"Read the security review"}
          </SiteLink>
        </article>
      </section>
      <section className="section callout">
        <h2>{"Self-hosting includes responsibility."}</h2>
        <p>
          {
            "Your team manages TLS, secrets, database durability, backups, upgrades and provider configuration. Provider credentials and live sandbox checks are still needed before production use. Paymux is under active development; this site makes no compliance certification or uptime guarantee."
          }
        </p>
      </section>
      <section className="section closing">
        <h2>
          {"Start small."}
          <br />
          {"Understand every moving part."}
        </h2>
        <SiteButton className="button" href="/docs">
          {"Run your first test payment"}
        </SiteButton>
      </section>
    </>
  );
}
