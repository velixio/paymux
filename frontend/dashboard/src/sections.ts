import { textOf, type ViewNode } from "./adapter";

export type Section = { title: string; slug: string; nodes: ViewNode[] };
export type SplitPage = {
  /** The h1 and what hangs off it (back link, description, actions). */
  head: ViewNode[];
  /** Everything before the first section heading. */
  details: ViewNode[];
  sections: Section[];
};

export function slugify(text: string): string {
  return (
    text
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "") || "section"
  );
}

const isHeading = (n: ViewNode) => n.tag === "h2" || n.tag === "h3";
const hasClass = (n: ViewNode, name: string) =>
  (n.attrs.class ?? "").split(/\s+/).includes(name);
// Nodes that belong to the page header, after the h1.
const isHeadExtra = (n: ViewNode) =>
  (n.tag === "p" && hasClass(n, "muted")) ||
  (n.tag === "nav" && hasClass(n, "overview-actions")) ||
  (n.tag === "a" && hasClass(n, "btn"));

/** The h1 header of a page (back link, h1, description, actions) and the rest. */
export function splitHead(nodes: ViewNode[]): {
  head: ViewNode[];
  rest: ViewNode[];
} {
  const h1 = nodes.findIndex((n) => n.tag === "h1");
  if (h1 < 0) return { head: [], rest: nodes };
  return { ...cut(nodes, h1, nodes.length) };
}

function cut(nodes: ViewNode[], h1: number, limit: number) {
  let end = h1 + 1;
  while (end < limit && isHeadExtra(nodes[end])) end++;
  return { head: nodes.slice(0, end), rest: nodes.slice(end) };
}

/**
 * Splits a record page at its h2/h3 headings. Returns null with fewer than two
 * sections, so such pages render as they always did.
 */
export function splitSections(nodes: ViewNode[]): SplitPage | null {
  const firstHeading = nodes.findIndex(isHeading);
  if (firstHeading < 0) return null;
  if (nodes.filter(isHeading).length < 2) return null;

  const h1 = nodes.findIndex((n) => n.tag === "h1");
  const headEnd =
    h1 >= 0 && h1 < firstHeading ? cut(nodes, h1, firstHeading).head.length : 0;

  const sections: Section[] = [];
  const used = new Set<string>();
  nodes.slice(firstHeading).forEach((n) => {
    if (isHeading(n)) {
      const text = textOf(n).trim();
      const base = slugify(text);
      let slug = base;
      for (let i = 2; used.has(slug); i++) slug = `${base}-${i}`;
      used.add(slug);
      sections.push({
        title: text || `Section ${sections.length + 1}`,
        slug,
        nodes: [],
      });
    } else sections[sections.length - 1].nodes.push(n);
  });
  return {
    head: nodes.slice(0, headEnd),
    details: nodes.slice(headEnd, firstHeading),
    sections,
  };
}

/** Title and status of a record page, from its h1 (the status is a span). */
export function recordHeading(nodes: ViewNode[]): {
  title: string;
  status: string;
} {
  const h1 = nodes.find((n) => n.tag === "h1");
  if (!h1) return { title: "", status: "" };
  const status = h1.children.find((c) =>
    (c.attrs.class ?? "").split(/\s+/).includes("status"),
  );
  const title = h1.children
    .filter((c) => c !== status && !hasClass(c, "mode"))
    .map(textOf)
    .join("")
    .trim();
  return { title, status: status ? textOf(status).trim() : "" };
}
