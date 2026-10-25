/** Presentation adapter for existing same-origin Go pages. No new API or browser API key. */
export type ViewNode = {
  tag: string;
  attrs: Record<string, string>;
  children: ViewNode[];
  text?: string;
};
export type DashboardPage = {
  focusSetting?: string;
  href: string;
  title: string;
  user: string;
  mode: string;
  nav: { href: string; label: string; active: boolean; count?: number }[];
  content: ViewNode[];
  modeForm: ViewNode | null;
  logoutForm: ViewNode | null;
  warnings: string[];
  auth: boolean;
  /** The page shows a one-time secret: never keep it mounted or cached. */
  once: boolean;
  theme: string | null;
  /** The dashboard theme this page is drawn in (data-dashboard-theme on the react root). */
  dashboardTheme: string | null;
  /** Changes whenever that theme's files do (data-dashboard-theme-version). */
  dashboardThemeVersion: string | null;
};
const ignored = new Set(["script", "style", "link", "noscript"]);
function node(el: Node): ViewNode | null {
  if (el.nodeType === Node.TEXT_NODE)
    return {
      tag: "#text",
      attrs: {},
      children: [],
      text: el.textContent || "",
    };
  if (!(el instanceof Element) || ignored.has(el.tagName.toLowerCase()))
    return null;
  const attrs = Object.fromEntries(
    [...el.attributes]
      .filter((a) => !a.name.startsWith("on"))
      .map((a) => [a.name, a.value]),
  );
  if (
    el.tagName === "INPUT" &&
    ["date", "month"].includes(attrs.type) &&
    !(el as HTMLInputElement).labels?.length
  )
    attrs["data-date-label"] = attrs["aria-label"] || attrs.name || "Date";
  if (
    ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName) &&
    attrs.type !== "hidden" &&
    !(el as HTMLInputElement).labels?.length &&
    !attrs["aria-label"] &&
    !attrs["aria-labelledby"]
  ) {
    const names: Record<string, string> = {
      notes: "Notes for the bank",
      file_1: "Evidence file 1",
      file_2: "Evidence file 2",
      file_3: "Evidence file 3",
      signed_qr: "Signed QR code",
    };
    const name = attrs.name?.replaceAll("_", " ");
    attrs["aria-label"] =
      names[attrs.name] ||
      (name
        ? name[0].toUpperCase() + name.slice(1)
        : attrs.placeholder || "Form value");
  }
  return {
    tag: el.tagName.toLowerCase(),
    attrs,
    children: [...el.childNodes].map(node).filter((n): n is ViewNode => !!n),
  };
}
export function parseDashboard(doc: Document, href: string): DashboardPage {
  const main =
    doc.querySelector(".dash-main") || doc.getElementById("paymux-server-page");
  if (!main) throw new Error("This page does not contain a Paymux workspace.");
  const parseForm = (selector: string) => {
    const el = doc.querySelector(selector);
    return el ? node(el) : null;
  };
  return {
    href,
    title: doc.title.replace(/ - Paymux$/, ""),
    user: doc.querySelector(".dash-head>.muted")?.textContent?.trim() || "",
    mode: doc.querySelector(".modeform .mode")?.textContent?.trim() || "",
    nav: [...doc.querySelectorAll(".dash-nav a")].map((a) => {
      const count = Number(a.getAttribute("data-count"));
      return {
        href: a.getAttribute("href")!,
        // the server's fallback badge is not part of the name
        label: [...a.childNodes]
          .filter((c) => !(c as Element).classList?.contains("badge"))
          .map((c) => c.textContent)
          .join("")
          .trim(),
        active: a.hasAttribute("aria-current"),
        ...(count > 0 ? { count } : {}),
      };
    }),
    content: [...main.childNodes].map(node).filter((n): n is ViewNode => !!n),
    modeForm: parseForm(".modeform"),
    logoutForm: parseForm('form[action="/dashboard/logout"]'),
    warnings: [...doc.querySelectorAll(".dev-banner,.banner-warning")].map(
      (e) => e.textContent!.trim(),
    ),
    auth: !doc.querySelector(".dash-head"),
    once: !!main.querySelector("[data-once]"),
    theme:
      doc.querySelector('link[href^="/theme/"]')?.getAttribute("href") || null,
    dashboardTheme:
      doc
        .querySelector("#paymux-react-root")
        ?.getAttribute("data-dashboard-theme") || null,
    dashboardThemeVersion:
      doc
        .querySelector("#paymux-react-root")
        ?.getAttribute("data-dashboard-theme-version") || null,
  };
}
export class SessionRedirect extends Error {}
export async function fetchDashboard(
  href: string,
  signal?: AbortSignal,
): Promise<DashboardPage> {
  const res = await fetch(href, {
    credentials: "same-origin",
    headers: { Accept: "text/html" },
    signal,
  });
  const type = res.headers.get("content-type") || "";
  if (!type.includes("text/html"))
    throw new Error(`The workspace returned ${res.status}. Try again.`);
  const html = await res.text();
  const doc = new DOMParser().parseFromString(html, "text/html");
  if (!doc.getElementById("paymux-server-page"))
    throw new Error(`Unable to load workspace (${res.status}).`);
  if (
    res.redirected &&
    /\/dashboard\/login/.test(new URL(res.url).pathname) &&
    !/\/dashboard\/login/.test(new URL(href, location.origin).pathname)
  ) {
    location.assign(res.url);
    throw new SessionRedirect("Your session expired. Sign in again.");
  }
  return parseDashboard(
    doc,
    new URL(res.url).pathname + new URL(res.url).search,
  );
}
export function textOf(n: ViewNode): string {
  return n.text ?? n.children.map(textOf).join("");
}
export function extractRows(n: ViewNode): ViewNode[] {
  return n.tag === "tr" ? [n] : n.children.flatMap(extractRows);
}
export function attributes(n: ViewNode): Record<string, unknown> {
  const props: Record<string, unknown> = {};
  const names: Record<string, string> = {
    class: "className",
    for: "htmlFor",
    tabindex: "tabIndex",
    colspan: "colSpan",
    rowspan: "rowSpan",
    maxlength: "maxLength",
    minlength: "minLength",
    readonly: "readOnly",
    autocomplete: "autoComplete",
    inputmode: "inputMode",
    spellcheck: "spellCheck",
    enctype: "encType",
    formaction: "formAction",
    accept: "accept",
  };
  const booleans = new Set([
    "required",
    "disabled",
    "multiple",
    "readOnly",
    "autoFocus",
    "open",
  ]);
  for (const [attr, value] of Object.entries(n.attrs)) {
    if (
      [
        "style",
        "selected",
        "checked",
        "value",
        "autofocus",
        "data-copy",
      ].includes(attr) ||
      attr.startsWith("on")
    )
      continue;
    const key = names[attr] || attr;
    props[key] = booleans.has(key) ? true : value;
  }
  return props;
}
export function preservePostedValues(
  page: DashboardPage,
  action: string,
  data: FormData,
) {
  function visit(n: ViewNode) {
    if (
      n.tag === "form" &&
      new URL(n.attrs.action || page.href, location.origin).pathname ===
        new URL(action, location.origin).pathname
    ) {
      const seen = new Map<string, number>();
      function control(el: ViewNode) {
        const name = el.attrs.name;
        if (
          name &&
          ["input", "textarea", "select"].includes(el.tag) &&
          data.has(name)
        ) {
          const values = data
            .getAll(name)
            .filter((v): v is string => typeof v === "string");
          const index = seen.get(name) || 0;
          seen.set(name, index + 1);
          if (["checkbox", "radio"].includes(el.attrs.type)) {
            if (values.includes(el.attrs.value || "on")) el.attrs.checked = "";
            else delete el.attrs.checked;
          } else if (el.tag === "select") {
            el.children.forEach((option) => {
              if (values.includes(option.attrs.value || textOf(option)))
                option.attrs.selected = "";
              else delete option.attrs.selected;
            });
          } else if (el.tag === "textarea") {
            el.children = [
              {
                tag: "#text",
                attrs: {},
                children: [],
                text: values[index] ?? values[0],
              },
            ];
          } else if (el.attrs.type !== "file")
            el.attrs.value = values[index] ?? values[0];
        }
        el.children.forEach(control);
      }
      n.children.forEach(control);
    } else n.children.forEach(visit);
  }
  page.content.forEach(visit);
  return page;
}

export function isDownloadPath(path: string): boolean {
  return (
    /\/(pdf|reports\/(file|zip))$/.test(path) ||
    /\.(json|csv|zip|pdf)$/.test(path) ||
    /\/disputes\/[^/]+\/evidence\/[^/]+$/.test(path)
  );
}
