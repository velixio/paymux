import { useEffect, useId, useState, type ReactNode } from "react";
import { useLocation } from "@tanstack/react-router";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import {
  ArrowLeft,
  ArrowRight,
  ArrowUpRight,
  BookOpen,
  Search,
  X,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { CopyButton, SiteLink } from "./components";
import {
  documentLink,
  guidePath,
  guides,
  headings,
  searchGuides,
  sections,
} from "./docs/catalog";
import "./docs.css";

const rawGuides = import.meta.glob<string>("../../../docs/*.md", {
  query: "?raw",
  import: "default",
  eager: true,
});
const content: Record<string, string> = Object.fromEntries(
  Object.entries(rawGuides).map(([file, text]) => [
    file.replace(/^(\.\.\/)+/, ""),
    text,
  ]),
);

function CodeBlock({ children }: { children?: ReactNode }) {
  const id = useId();
  return (
    <div className="docs-code">
      <div>
        <span>Example</span>
        <CopyButton target={id} />
      </div>
      <pre id={id} tabIndex={0} aria-label="Scrollable code example">
        {children}
      </pre>
    </div>
  );
}

export default function Documentation() {
  const pathname = useLocation({ select: (location) => location.pathname });
  const slug = pathname.replace(/^\/docs\/?/, "").replace(/\/$/, "");
  const guide = guides.find((item) => item.slug === slug);
  const [query, setQuery] = useState("");
  const [navigationOpen, setNavigationOpen] = useState(false);
  const source = guide ? content[guide.source] : "";
  const outline = headings(source || "");
  const results = searchGuides(query, content);
  const index = guide ? guides.indexOf(guide) : -1;
  useEffect(() => {
    setQuery("");
    setNavigationOpen(false);
    document.title = `${guide?.title ?? "Guide not found"} · Paymux documentation`;
  }, [pathname, guide]);
  // Keep /docs#routing and /docs#webhooks links from older site versions useful.
  useEffect(() => {
    if (location.hash)
      requestAnimationFrame(() =>
        document
          .getElementById(decodeURIComponent(location.hash.slice(1)))
          ?.scrollIntoView(),
      );
  }, [pathname]);
  if (!guide || !source)
    return (
      <section className="page-intro">
        <h1>Guide not found</h1>
        <p>This documentation page does not exist.</p>
        <SiteLink href="/docs">Browse the documentation</SiteLink>
      </section>
    );
  const h2 = outline.filter((heading) => heading.level === 2);
  const heading = (level: 1 | 2 | 3, children: ReactNode, line?: number) => {
    const id = outline.find((item) => item.line === line)?.id;
    if (level === 1) return <h1 id={id}>{children}</h1>;
    if (level === 2)
      return (
        <h2 id={id}>
          <a href={`#${id}`}>{children}</a>
        </h2>
      );
    return <h3 id={id}>{children}</h3>;
  };
  return (
    <div className="documentation-layout">
      <aside className="docs-sidebar">
        <div className="docs-brand">
          <BookOpen aria-hidden="true" size={18} />
          <span>Documentation</span>
        </div>
        <div className="docs-search">
          <label className="sr-only" htmlFor="docs-search">
            Search documentation
          </label>
          <Search size={16} aria-hidden="true" />
          <Input
            type="search"
            id="docs-search"
            name="docs-search"
            autoComplete="off"
            placeholder="Search the guides…"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
          />
          {query && (
            <Button
              size="icon"
              variant="ghost"
              aria-label="Clear documentation search"
              onClick={() => setQuery("")}
            >
              <X size={14} />
            </Button>
          )}
        </div>
        <Button
          variant="outline"
          className="docs-mobile-toggle"
          onClick={() => setNavigationOpen((value) => !value)}
          aria-expanded={navigationOpen}
          aria-controls="docs-navigation"
        >
          {navigationOpen ? "Hide guides" : "Browse all guides"}
        </Button>
        <nav
          id="docs-navigation"
          className={
            navigationOpen ? "docs-navigation is-open" : "docs-navigation"
          }
          aria-label="Documentation guides"
        >
          {sections.map((section) => (
            <div key={section}>
              <span>{section}</span>
              {guides
                .filter((item) => item.section === section)
                .map((item) => (
                  <SiteLink
                    key={item.slug}
                    exact
                    href={guidePath(item)}
                    aria-current={guide.slug === item.slug ? "page" : undefined}
                  >
                    {item.title}
                  </SiteLink>
                ))}
            </div>
          ))}
        </nav>
        <a
          className="docs-source-link"
          href="https://github.com/velixio/paymux"
        >
          <ArrowUpRight size={14} aria-hidden="true" /> View the source
        </a>
      </aside>
      <div className="docs-center">
        {query.trim() ? (
          <section className="docs-results" aria-labelledby="search-title">
            <span className="docs-breadcrumb">Documentation / Search</span>
            <h1 id="search-title">Search the documentation</h1>
            <p role="status" aria-live="polite">
              {results.length} {results.length === 1 ? "guide" : "guides"}{" "}
              matching “{query.trim()}”
            </p>
            {results.length ? (
              <div>
                {results.map((item) => (
                  <SiteLink
                    href={guidePath(item)}
                    key={item.slug}
                    onClick={() => setQuery("")}
                  >
                    <span>{item.section}</span>
                    <h2>
                      {item.title} <ArrowRight size={17} aria-hidden="true" />
                    </h2>
                    <p>{item.description}</p>
                  </SiteLink>
                ))}
              </div>
            ) : (
              <p>
                Try a term such as “webhook”, “refund”, “master key”, or
                “routing”.
              </p>
            )}
          </section>
        ) : (
          <>
            <div className="docs-breadcrumb">
              <SiteLink href="/docs">Docs</SiteLink>
              <span>/</span>
              {guide.section}
            </div>
            <article className="docs-article">
              <Markdown
                remarkPlugins={[remarkGfm]}
                skipHtml
                components={{
                  h1: ({ children, node }) =>
                    heading(1, children, node?.position?.start.line),
                  h2: ({ children, node }) =>
                    heading(2, children, node?.position?.start.line),
                  h3: ({ children, node }) =>
                    heading(3, children, node?.position?.start.line),
                  a: ({ href = "", children }) => (
                    <SiteLink href={documentLink(href, guide.source)}>
                      {children}
                    </SiteLink>
                  ),
                  pre: ({ children }) => <CodeBlock>{children}</CodeBlock>,
                  table: ({ children }) => (
                    <div
                      className="docs-table-scroll"
                      tabIndex={0}
                      role="region"
                      aria-label="Scrollable reference table"
                    >
                      <table>{children}</table>
                    </div>
                  ),
                }}
              >
                {source}
              </Markdown>
            </article>
            {guide.slug === "" && (
              <div className="docs-legacy-anchors">
                <span id="routing" />
                <SiteLink href="/docs/payments">
                  Read the complete payments and routing guide{" "}
                  <ArrowRight size={15} />
                </SiteLink>
                <span id="webhooks" />
                <SiteLink href="/docs/webhooks">
                  Read the complete webhook guide <ArrowRight size={15} />
                </SiteLink>
              </div>
            )}
            <div className="docs-article-meta">
              <span>Checked against repository source</span>
              <a
                href={`https://github.com/velixio/paymux/blob/main/${guide.source}`}
              >
                Edit this guide <ArrowUpRight size={14} aria-hidden="true" />
              </a>
            </div>
            <nav className="docs-pagination" aria-label="Adjacent guides">
              {index > 0 ? (
                <SiteLink href={guidePath(guides[index - 1])}>
                  <small>
                    <ArrowLeft size={13} aria-hidden="true" /> Previous
                  </small>
                  {guides[index - 1].title}
                </SiteLink>
              ) : (
                <span />
              )}
              {index < guides.length - 1 && (
                <SiteLink href={guidePath(guides[index + 1])}>
                  <small>
                    Next <ArrowRight size={13} aria-hidden="true" />
                  </small>
                  {guides[index + 1].title}
                </SiteLink>
              )}
            </nav>
          </>
        )}
      </div>
      {!query.trim() && h2.length > 0 && (
        <aside className="docs-outline">
          <span>On this page</span>
          <nav aria-label="On this page">
            {h2.map((item) => (
              <a href={`#${item.id}`} key={item.id}>
                {item.text}
              </a>
            ))}
          </nav>
          <a
            className="docs-api-link"
            href="https://github.com/velixio/paymux/blob/main/docs/openapi.yaml"
          >
            OpenAPI specification <ArrowUpRight size={13} aria-hidden="true" />
          </a>
        </aside>
      )}
    </div>
  );
}
