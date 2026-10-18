import catalog from "../../docs-catalog.json";

export type Guide = (typeof catalog)[number];
export const guides: Guide[] = catalog;
export const guidePath = (guide: Guide) =>
  guide.slug ? `/docs/${guide.slug}` : "/docs";
export const sections = [...new Set(guides.map((guide) => guide.section))];

export function headingSlug(text: string): string {
  return text
    .toLowerCase()
    .replace(/[`*]/g, "")
    .replace(/[^\p{L}\p{N}\s_-]/gu, "")
    .replace(/\s/g, "-");
}

export function headings(markdown: string) {
  // Fenced examples can contain lines starting with #; they are not headings.
  const withoutCode = markdown.replace(
    /^```[^\n]*\n[\s\S]*?^```[ \t]*$/gm,
    (block) => "\n".repeat(block.split("\n").length - 1),
  );
  const counts = new Map<string, number>();
  return [...withoutCode.matchAll(/^(#{1,3})\s+(.+)$/gm)].map((match) => {
    const text = match[2].trim();
    const base = headingSlug(text);
    const count = counts.get(base) ?? 0;
    counts.set(base, count + 1);
    return {
      line: withoutCode.slice(0, match.index).split("\n").length,
      level: match[1].length,
      text: text.replace(/[`*]/g, ""),
      id: count ? `${base}-${count}` : base,
    };
  });
}

export function documentLink(href: string, source: string): string {
  if (!href || href.startsWith("#") || /^(https?:|mailto:)/i.test(href))
    return href;
  // Existing guides use docs/foo.md in a few root-relative prose links.
  const relative = href.startsWith("docs/") ? `../${href}` : href;
  const url = new URL(relative, `https://source.invalid/${source}`);
  if (url.origin !== "https://source.invalid") return href;
  const path = decodeURIComponent(url.pathname.slice(1));
  const guide = guides.find((item) => item.source === path);
  return guide
    ? `${guidePath(guide)}${url.hash}`
    : `https://github.com/velixio/paymux/blob/main/${path}${url.hash}`;
}

export function searchGuides(query: string, content: Record<string, string>) {
  const terms = query.toLowerCase().trim().split(/\s+/).filter(Boolean);
  if (!terms.length) return [];
  return guides.filter((guide) => {
    const haystack =
      `${guide.title} ${guide.description} ${content[guide.source] ?? ""}`.toLowerCase();
    return terms.every((term) => haystack.includes(term));
  });
}
