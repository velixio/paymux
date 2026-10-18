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
        <p className="eyebrow">{"Provider connections"}</p>
        <h1>
          {"One hub."}
          <br />
          {"Different capabilities."}
        </h1>
        <p className="lead">
          {
            "Choose a provider for what it actually supports. Paymux keeps provider-specific differences visible."
          }
        </p>
      </section>
      <section className="section compact">
        <div
          className="table-wrap"
          tabIndex={0}
          aria-label="Scrollable provider capabilities"
        >
          <Table>
            <caption>{"Declared connector capabilities"}</caption>
            <TableHeader>
              <TableRow>
                <TableHead>{"Provider"}</TableHead>
                <TableHead>{"Refunds"}</TableHead>
                <TableHead>
                  {"Disputes"}
                  <br />
                  {"Read / respond"}
                </TableHead>
                <TableHead>{"Reconciliation"}</TableHead>
                <TableHead>{"Settlements"}</TableHead>
                <TableHead>{"Recurring billing"}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              <TableRow>
                <TableHead>{"Razorpay"}</TableHead>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"Yes / Yes"}</TableCell>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"Provider-managed"}</TableCell>
              </TableRow>
              <TableRow>
                <TableHead>{"Stripe"}</TableHead>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"Yes / Yes"}</TableCell>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"Provider-managed"}</TableCell>
              </TableRow>
              <TableRow>
                <TableHead>{"PayU India"}</TableHead>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
                <TableCell>{"Merchant-triggered mandates"}</TableCell>
              </TableRow>
              <TableRow>
                <TableHead>{"Whop"}</TableHead>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
                <TableCell>{"Provider-managed"}</TableCell>
              </TableRow>
              <TableRow>
                <TableHead>{"Zoho Payments"}</TableHead>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
                <TableCell>{"Merchant-triggered mandates"}</TableCell>
              </TableRow>
              <TableRow>
                <TableHead>{"Cashfree"}</TableHead>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
                <TableCell>{"Yes"}</TableCell>
                <TableCell>{"\u2014"}</TableCell>
              </TableRow>
              <TableRow>
                <TableHead>{"Your HTTP service"}</TableHead>
                <TableCell colSpan={5}>
                  {
                    "Capabilities are declared by your remote connector service."
                  }
                </TableCell>
              </TableRow>
            </TableBody>
          </Table>
        </div>
        <div className="callout">
          <strong>
            {"Implemented capability and live verification are separate."}
          </strong>
          <p>
            {"This table reflects connector declarations. Check the "}
            <SiteLink href="https://github.com/velixio/paymux/blob/main/docs/provider-verification.md">
              {"provider verification checklist"}
            </SiteLink>
            {
              " before relying on a workflow in production. A dash means the capability is not declared."
            }
          </p>
        </div>
        <p>
          {
            "Whop acts as merchant of record; Paymux does not issue GST invoices for Whop payments. Payment methods, currencies and mandate caps also vary by connector."
          }
        </p>
        <SiteLink
          className="text-link"
          href="https://github.com/velixio/paymux/blob/main/docs/capabilities.md"
        >
          {"Read the complete generated capability matrix"}
        </SiteLink>
      </section>
      <section className="section developer">
        <div>
          <h2>
            {"Your provider,"}
            <br />
            {"your connector."}
          </h2>
          <p>
            {
              "Use the public Go connector SDK or connect a service written in another language through the signed HTTP protocol."
            }
          </p>
        </div>
        <SiteButton
          className="button"
          href="https://github.com/velixio/paymux/blob/main/docs/connectors.md"
        >
          {"Build a connector"}
        </SiteButton>
      </section>
    </>
  );
}
