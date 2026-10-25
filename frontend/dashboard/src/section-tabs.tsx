import { useState, type ReactNode } from "react";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { ViewNode } from "./adapter";
import { splitSections, type SplitPage } from "./sections";
import "./section-tabs.css";

const DETAILS = "details";

/** The split of a record page, or null when it should render as it is. */
export function recordSplit(nodes: ViewNode[]): SplitPage | null {
  return splitSections(nodes);
}

function fromHash(split: SplitPage): string {
  const slug = location.hash.replace(/^#tab-/, "");
  return location.hash.startsWith("#tab-") &&
    split.sections.some((s) => s.slug === slug)
    ? slug
    : DETAILS;
}

/**
 * Inner tabs of a record page: Details, then one tab per section. Every panel
 * stays mounted (hidden when not selected), so edits in a form, dirty tracking
 * and one-time secrets survive switching. With syncHash the selection is also
 * kept in the URL as #tab-<slug>.
 */
export function SectionTabs({
  split,
  render,
  syncHash,
}: {
  split: SplitPage;
  render: (nodes: ViewNode[]) => ReactNode;
  syncHash?: boolean;
}) {
  const [value, setValue] = useState(() =>
    syncHash ? fromHash(split) : DETAILS,
  );
  const select = (next: string) => {
    setValue(next);
    if (!syncHash) return;
    const url =
      location.pathname +
      location.search +
      (next === DETAILS ? "" : `#tab-${next}`);
    history.replaceState(history.state, "", url);
  };
  const panels = [
    { slug: DETAILS, title: "Details", nodes: split.details },
    ...split.sections,
  ];
  return (
    <>
      {render(split.head)}
      <Tabs value={value} onValueChange={select} className="section-tabs">
        <TabsList
          variant="line"
          className="section-tabs-list"
          aria-label="Sections"
        >
          {panels.map((p) => (
            <TabsTrigger key={p.slug} value={p.slug}>
              {p.title}
            </TabsTrigger>
          ))}
        </TabsList>
        {panels.map((p) => (
          <TabsContent key={p.slug} value={p.slug} forceMount>
            <div className="section-tabs-panel" hidden={p.slug !== value}>
              {render(p.nodes)}
            </div>
          </TabsContent>
        ))}
      </Tabs>
    </>
  );
}
