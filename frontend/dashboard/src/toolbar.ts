import { textOf, type ViewNode } from "./adapter";

export type ToolbarModel = {
  back: { href: string; label: string } | null;
  /** The h1's content without the inline mode span. */
  title: ViewNode[];
  description: string | null;
  /** Links and buttons for the right of the bar; the first is primary. */
  actions: ViewNode[];
  statuses: {
    options: { value: string; label: string; selected: boolean }[];
  } | null;
};

const blank = (n: ViewNode) => n.tag === "#text" && !(n.text ?? "").trim();
const has = (n: ViewNode, cls: string) =>
  (n.attrs.class ?? "").split(/\s+/).includes(cls);
const find = (n: ViewNode, test: (n: ViewNode) => boolean): ViewNode | null =>
  test(n) ? n : (n.children.map((c) => find(c, test)).find(Boolean) ?? null);
const findAll = (n: ViewNode, test: (n: ViewNode) => boolean): ViewNode[] =>
  (test(n) ? [n] : []).concat(n.children.flatMap((c) => findAll(c, test)));

function backLink(n: ViewNode) {
  if (n.tag !== "p") return null;
  const kids = n.children.filter((c) => !blank(c));
  const link = kids.length === 1 && kids[0].tag === "a" ? kids[0] : null;
  const text = link ? textOf(link).trim() : "";
  return link && text.startsWith("←")
    ? { href: link.attrs.href ?? "", label: text.slice(1).trim() }
    : null;
}

const filterForm = (rest: ViewNode[]) =>
  rest.find(
    (n) =>
      n.tag === "form" &&
      has(n, "filters") &&
      (n.attrs.method ?? "get").toLowerCase() === "get",
  );

function dropStatusSelect(n: ViewNode): ViewNode | null {
  if (n.tag === "select" && n.attrs.name === "status") {
    const chosen = findAll(
      n,
      (o) => o.tag === "option" && "selected" in o.attrs,
    )
      .map((o) => o.attrs.value ?? textOf(o).trim())
      .find((v) => v !== "");
    return chosen
      ? {
          tag: "input",
          attrs: { type: "hidden", name: "status", value: chosen },
          children: [],
        }
      : null;
  }
  return {
    ...n,
    children: n.children
      .map(dropStatusSelect)
      .filter((c): c is ViewNode => c !== null),
  };
}

function statusChips(rest: ViewNode[]): ToolbarModel["statuses"] {
  const form = filterForm(rest);
  const select =
    form && find(form, (n) => n.tag === "select" && n.attrs.name === "status");
  if (!select) return null;
  const options = findAll(select, (n) => n.tag === "option").map((o) => ({
    value: o.attrs.value ?? textOf(o).trim(),
    label: textOf(o).trim(),
    selected: "selected" in o.attrs,
  }));
  const chosen = options.some((o) => o.value !== "" && o.selected);
  const all = { value: "", label: "All", selected: !chosen };
  return {
    options: [all, ...options.filter((o) => o.value !== "")],
  };
}

// Splits a page's content into the screen toolbar (back link, title,
// description, actions, status chips) and what stays in the page. A page that
// does not open with a title gets no toolbar.
export function extractToolbar(
  nodes: ViewNode[],
): { toolbar: ToolbarModel; rest: ViewNode[] } | null {
  const list = nodes.filter((n) => !blank(n));
  let i = 0;
  const back = list[i] ? backLink(list[i]) : null;
  if (back) i++;
  const h1 = list[i];
  if (!h1 || h1.tag !== "h1") return null;
  i++;

  const title = h1.children.filter(
    (c) => !(c.tag === "span" && has(c, "mode")),
  );
  while (title.length && blank(title[title.length - 1])) title.pop();
  while (title.length && blank(title[0])) title.shift();
  const last = title[title.length - 1];
  if (last?.tag === "#text")
    title[title.length - 1] = { ...last, text: (last.text ?? "").trimEnd() };

  let description: string | null = null;
  if (list[i]?.tag === "p" && has(list[i], "muted")) {
    description = textOf(list[i]).trim();
    i++;
  }
  const actions: ViewNode[] = [];
  const isButtonLink = (n: ViewNode) => n.tag === "a" && has(n, "btn");
  for (; i < list.length; i++) {
    const n = list[i];
    const inner = n.tag === "p" ? n.children.filter((c) => !blank(c)) : [];
    if (n.tag === "nav" && has(n, "overview-actions"))
      actions.push(
        ...n.children.filter((c) => c.tag === "a" || c.tag === "button"),
      );
    else if (isButtonLink(n)) actions.push(n);
    else if (inner.length === 1 && isButtonLink(inner[0]))
      actions.push(inner[0]);
    // A paragraph that is only a link (e.g. "Logs for this payment").
    else if (
      inner.length === 1 &&
      inner[0].tag === "a" &&
      textOf(n).trim() === textOf(inner[0]).trim()
    )
      actions.push(inner[0]);
    else break;
  }
  let rest = list.slice(i);
  const statuses = statusChips(rest);
  // The chips replace the form's own status control; the current status still
  // travels with the form's other filters.
  const form = filterForm(rest);
  if (statuses && form)
    rest = rest.map((n) => (n === form ? dropStatusSelect(n)! : n));
  return { toolbar: { back, title, description, actions, statuses }, rest };
}

/** Which edges of a horizontal scroller hide content (1px slack for rounding). */
export function overflowEdges(
  scrollLeft: number,
  scrollWidth: number,
  clientWidth: number,
): { start: boolean; end: boolean } {
  return {
    start: scrollLeft > 1,
    end: scrollLeft + clientWidth < scrollWidth - 1,
  };
}
