import { useEffect, useId, useMemo, useState } from "react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { textOf, type DashboardPage, type ViewNode } from "./adapter";
import { intents, rank, recent, remember, type Item } from "./command";
import { useRuntime } from "./runtime";
import { useTabs } from "./tabs-context";

const storage = (): Storage | null => {
  try {
    return localStorage;
  } catch {
    return null;
  }
};

const find = (n: ViewNode, test: (n: ViewNode) => boolean): ViewNode | null => {
  if (test(n)) return n;
  for (const c of n.children) {
    const hit = find(c, test);
    if (hit) return hit;
  }
  return null;
};

// "Switch to test" comes from the server's own mode form (its button text).
function modeAction(form: ViewNode | null) {
  const button =
    form && find(form, (n) => n.tag === "button" && !!n.attrs.name);
  if (!form || !button) return null;
  return {
    label: textOf(button).trim() || `Switch to ${button.attrs.value}`,
    // A detached copy of the form, submitted through the workspace runtime.
    build() {
      const el = document.createElement("form");
      el.setAttribute("action", form.attrs.action ?? "");
      el.setAttribute("method", form.attrs.method ?? "post");
      const hidden = (n: ViewNode): ViewNode[] =>
        (n.tag === "input" && n.attrs.type === "hidden" ? [n] : []).concat(
          n.children.flatMap(hidden),
        );
      for (const h of hidden(form)) {
        const input = document.createElement("input");
        input.type = "hidden";
        input.name = h.attrs.name;
        input.value = h.attrs.value ?? "";
        el.append(input);
      }
      const submit = document.createElement("button");
      submit.name = button.attrs.name;
      submit.value = button.attrs.value ?? "";
      el.append(submit);
      return { form: el, button: submit };
    },
  };
}

type Row = { item: Item; group?: string };

export default function CommandBar({
  open,
  onOpenChange,
  page,
  onToggleTheme,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  page: DashboardPage;
  onToggleTheme: () => void;
}) {
  const api = useTabs();
  const { submit } = useRuntime();
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const listId = useId();
  const mode = useMemo(() => modeAction(page.modeForm), [page.modeForm]);

  useEffect(() => {
    if (open) {
      setQuery("");
      setActive(0);
    }
  }, [open]);

  const rows = useMemo<Row[]>(() => {
    const actions: Item[] = [
      {
        id: "action:create-link",
        label: "Create payment link",
        hint: "/dashboard/links",
        href: "/dashboard/links",
      },
      ...(mode
        ? [
            {
              id: "action:mode",
              label: mode.label,
              action: "switch-mode" as const,
            },
          ]
        : []),
      {
        id: "action:theme",
        label: "Toggle theme",
        action: "toggle-theme" as const,
      },
      ...(api.state.closed.length
        ? [
            {
              id: "action:reopen",
              label: "Reopen closed tab",
              action: "reopen-tab" as const,
            },
          ]
        : []),
    ];
    const pages: Item[] = page.nav.map((n) => ({
      id: `page:${n.href}`,
      label: n.label,
      hint: n.href,
      href: n.href,
    }));
    const open: Item[] = api.state.tabs.map((t) => ({
      id: `tab:${t.id}`,
      label: t.title,
      hint: "Open tab",
      href: t.href,
    }));
    const seen = recent(storage(), page.user);
    if (!query.trim()) {
      const ids = new Set(seen.map((i) => i.id));
      return [
        ...seen.map((item) => ({ item, group: "Recent" })),
        ...actions
          .filter((i) => !ids.has(i.id))
          .map((item) => ({ item, group: "Actions" })),
        ...pages
          .filter((i) => !ids.has(i.id))
          .map((item) => ({ item, group: "Pages" })),
        ...open.map((item) => ({ item, group: "Open tabs" })),
      ].map((r, i, all) => ({
        ...r,
        group: all[i - 1]?.group === r.group ? undefined : r.group,
      }));
    }
    // A record id goes first; "search for ..." fallbacks come after real matches.
    const found = intents(query);
    return [
      ...found.filter((i) => i.id.startsWith("record:")),
      ...rank([...actions, ...pages, ...open, ...seen], query),
      ...found.filter((i) => !i.id.startsWith("record:")),
    ]
      .filter((item, i, all) => all.findIndex((x) => x.id === item.id) === i)
      .map((item) => ({ item }));
  }, [
    query,
    mode,
    page.nav,
    page.user,
    api.state.tabs,
    api.state.closed.length,
  ]);

  const at = Math.min(active, rows.length - 1);

  function run(item: Item, alt: boolean) {
    remember(storage(), page.user, item);
    onOpenChange(false);
    if (item.href) api.openHref(item.href, alt ? "new" : "replace");
    else if (item.action === "toggle-theme") onToggleTheme();
    else if (item.action === "reopen-tab") api.reopen();
    else if (item.action === "switch-mode" && mode) {
      const { form, button } = mode.build();
      submit(form, button);
    }
  }

  function onKeyDown(e: React.KeyboardEvent) {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const step = e.key === "ArrowDown" ? 1 : -1;
      setActive((at + step + rows.length) % rows.length);
    } else if (e.key === "Enter" && rows[at]) {
      e.preventDefault();
      run(rows[at].item, e.altKey);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="command-bar" showCloseButton={false}>
        <DialogTitle className="sr-only">Command bar</DialogTitle>
        <DialogDescription className="sr-only">
          Search pages, records and actions. Enter opens, Alt+Enter opens in a
          new tab.
        </DialogDescription>
        <input
          className="command-input"
          role="combobox"
          aria-label="Search pages, records and actions"
          aria-expanded="true"
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={rows[at] ? `${listId}-${at}` : undefined}
          placeholder="Search pages, payment ids, actions…"
          value={query}
          onChange={(e) => {
            setQuery(e.target.value);
            setActive(0);
          }}
          onKeyDown={onKeyDown}
          autoComplete="off"
          spellCheck={false}
        />
        <ul className="command-list" role="listbox" id={listId}>
          {rows.length === 0 && (
            <li role="presentation" className="command-empty">
              No matches
            </li>
          )}
          {rows.map(({ item, group }, i) => (
            <li key={item.id} role="presentation">
              {group && <div className="command-group">{group}</div>}
              <div
                role="option"
                id={`${listId}-${i}`}
                aria-selected={i === at}
                className={cn("command-option", i === at && "active")}
                onMouseMove={() => setActive(i)}
                onClick={(e) => run(item, e.altKey)}
              >
                <span>{item.label}</span>
                {item.hint && <small>{item.hint}</small>}
              </div>
            </li>
          ))}
        </ul>
        <p className="command-help" aria-hidden="true">
          ↑↓ move · Enter open · Alt+Enter new tab · Esc close
        </p>
      </DialogContent>
    </Dialog>
  );
}
