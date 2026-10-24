import React, {
  Suspense,
  lazy,
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  createBrowserHistory,
  createRootRoute,
  createRoute,
  createRouter,
  useLocation,
  useRouter,
  type RouterHistory,
} from "@tanstack/react-router";
import { useIsFetching, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowUpRight,
  Menu,
  PanelLeftClose,
  PanelLeftOpen,
  RefreshCw,
  Sun,
  Moon,
  CreditCard,
  House,
  Settings,
  Layers,
  FileText,
  ShieldCheck,
  Building2,
  Webhook,
  ChevronRight,
  SlidersHorizontal,
  Search,
  Code,
  Link as LinkIcon,
  Repeat,
  Scale,
  BarChart3,
  Bell,
  ScrollText,
  Users,
  UserRoundCog,
  Palette,
  Coins,
  Landmark,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Sheet,
  SheetTrigger,
  SheetContent,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
import { Alert, AlertTitle, AlertDescription } from "@/components/ui/alert";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
} from "@/components/ui/dropdown-menu";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { fetchDashboard, type DashboardPage } from "./adapter";
import { PageContent, RenderNode } from "./render";
import { queryKey, RuntimeProvider, WorkspaceLink } from "./runtime";
import { createMatcher, isTyping, type Binding } from "./keys";
import { TabStrip } from "./tab-strip";
import { TabView } from "./tab-view";
import { TabsContext, useTabs, type TabsApi } from "./tabs-context";
import * as tabs from "./tabs";
import type { TabState } from "./tabs";

export { useTabs, type TabsApi };

// The command bar is a separate chunk, fetched when first opened.
const CommandBar = lazy(() => import("./command-bar"));

const noDirty: ReadonlySet<string> = new Set();
const storage = (): Storage | null => {
  try {
    return localStorage;
  } catch {
    return null;
  }
};
// Sign-in, reauthentication and setup pages never become tabs.
const isAuthHref = (href: string) =>
  /^\/(?:dashboard\/(?:login|logout|reauth)(?:[/?#]|$)|setup(?:[/?#]|$))/.test(
    href,
  );
const titleFor = (href: string, page: DashboardPage) => {
  if (href === page.href) return page.title;
  const last = new URL(href, location.origin).pathname.split("/").at(-1) || "";
  return last
    ? last[0].toUpperCase() + last.slice(1).replaceAll("-", " ")
    : "Page";
};
const hrefOf = (loc: { pathname: string; searchStr: string }) =>
  loc.pathname + loc.searchStr;

const sections = [
  { name: "Workspace", keys: ["overview", "payments", "payment-links"] },
  { name: "Billing", keys: ["subscriptions", "invoices"] },
  {
    name: "Operations",
    keys: ["disputes", "reconciliation", "reports", "alerts", "audit", "logs"],
  },
  { name: "Connections", keys: ["apps", "providers"] },
  {
    name: "Administration",
    keys: ["settings", "tax-entities", "fx", "appearance", "users", "roles"],
  },
];
const iconMap: Record<string, React.ComponentType<{ size?: number }>> = {
  overview: House,
  payments: CreditCard,
  "payment-links": LinkIcon,
  subscriptions: Repeat,
  invoices: FileText,
  disputes: ShieldCheck,
  apps: Code,
  providers: Building2,
  settings: Settings,
  reconciliation: Scale,
  reports: BarChart3,
  alerts: Bell,
  audit: ScrollText,
  users: Users,
  roles: UserRoundCog,
  appearance: Palette,
  fx: Coins,
  "tax-entities": Landmark,
};
const iconFor = (href: string) =>
  iconMap[href.split(/[?#]/)[0].split("/").at(-1)!] || Layers;
function Logo() {
  return (
    <WorkspaceLink
      href="/dashboard/overview"
      className="react-logo"
      aria-label="Paymux overview"
    >
      <span aria-hidden>
        <ArrowUpRight size={23} />
      </span>
      <b className="react-wordmark">paymux</b>
    </WorkspaceLink>
  );
}
function Navigation({
  page,
  close,
}: {
  page: DashboardPage;
  close?: () => void;
}) {
  return (
    <nav aria-label="Workspace navigation" className="react-navigation">
      {sections.map((section) => (
        <div className="navigation-group" key={section.name}>
          <span className="navigation-caption">{section.name}</span>
          {page.nav
            .filter((item) =>
              section.keys.includes(item.href.split("/").at(-1)!),
            )
            .map((item) => {
              const Icon = iconMap[item.href.split("/").at(-1)!] || Layers;
              return (
                <WorkspaceLink
                  href={item.href}
                  key={item.href}
                  onClick={close}
                  className={
                    "navigation-item " + (item.active ? "selected" : "")
                  }
                  aria-label={item.label}
                  title={item.label}
                  aria-current={item.active ? "page" : undefined}
                >
                  <Icon size={16} />
                  <span>{item.label}</span>
                  {item.active && <span className="active-marker" />}
                </WorkspaceLink>
              );
            })}
        </div>
      ))}
    </nav>
  );
}
function RefreshWorkspace({
  refresh,
  loading,
  hasEdits,
}: {
  refresh: () => void;
  loading: boolean;
  hasEdits: boolean;
}) {
  return (
    <Button
      variant="ghost"
      size="icon"
      title={
        hasEdits
          ? "Save or undo your changes before refreshing"
          : "Refresh workspace"
      }
      aria-label="Refresh workspace"
      onClick={refresh}
      disabled={loading || hasEdits}
    >
      <RefreshCw size={16} className={loading ? "refreshing" : ""} />
    </Button>
  );
}
// Page chrome that outlives tabs: title, theme stylesheet, dark class and the
// readiness flag. It reads the page of the active tab.
function useChrome(page: DashboardPage | null) {
  const [dark, setDark] = useState(() => {
    try {
      return (
        localStorage.getItem("paymux-theme") === "dark" ||
        (!localStorage.getItem("paymux-theme") &&
          (!!page?.theme?.includes("/minimal-dark/") ||
            matchMedia("(prefers-color-scheme:dark)").matches))
      );
    } catch {
      return false;
    }
  });
  const title = page?.title;
  useEffect(() => {
    if (title === undefined) return;
    document.title = title + " - Paymux";
    const source = document.getElementById("paymux-server-page");
    if (source) {
      source.textContent = "";
      source.remove();
    }
    document
      .getElementById("paymux-react-root")
      ?.setAttribute("data-ready", "true");
  }, [title, page?.href]);
  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
  }, [dark]);
  const theme = page?.theme ?? null;
  useEffect(() => {
    if (!page) return;
    let link = document.querySelector<HTMLLinkElement>('link[href^="/theme/"]');
    if (theme) {
      if (!link) {
        link = document.createElement("link");
        link.rel = "stylesheet";
        document.head.append(link);
      }
      link.href = theme;
    } else link?.remove();
    try {
      if (!localStorage.getItem("paymux-theme"))
        setDark(
          !!theme?.includes("/minimal-dark/") ||
            matchMedia("(prefers-color-scheme:dark)").matches,
        );
    } catch {}
    // Only a change of the theme link re-evaluates the default.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [theme, page !== null]);
  return { dark, setDark };
}

const modeOf = (page: DashboardPage) =>
  page.mode.includes("test") ? "test" : "live";

function AuthLayout({ page }: { page: DashboardPage }) {
  return (
    <RuntimeProvider page={page}>
      <div data-mode={modeOf(page)} className="paymux-react auth-workspace">
        <a href="#react-workspace" className="react-skip">
          Skip to workspace
        </a>
        <div className="auth-container">
          <Logo />
          <div className="auth-card">
            <PageContent key={page.href} nodes={page.content} />
          </div>
          <p className="auth-footnote">Self-hosted payment infrastructure</p>
        </div>
      </div>
    </RuntimeProvider>
  );
}

function TabsShell({
  slot,
  page,
  sessionKey,
  dark,
  setDark,
}: {
  slot?: React.ReactNode;
  page: DashboardPage;
  sessionKey: string;
  dark: boolean;
  setDark: (dark: boolean) => void;
}) {
  const router = useRouter();
  const client = useQueryClient();
  const loc = useLocation();
  const href = hrefOf(loc);
  const [menu, setMenu] = useState(false);
  const [commandOpen, setCommandOpen] = useState(false);
  const [commandLoaded, setCommandLoaded] = useState(false);
  const openCommand = useCallback(() => {
    setCommandLoaded(true);
    setCommandOpen(true);
  }, []);
  const toggleTheme = () => {
    setDark(!dark);
    try {
      localStorage.setItem("paymux-theme", !dark ? "dark" : "light");
    } catch {}
  };
  const [collapsed, setCollapsed] = useState(() => {
    try {
      return localStorage.getItem("paymux-sidebar-collapsed") === "true";
    } catch {
      return false;
    }
  });
  const [dirtyIds, setDirtyIds] = useState<ReadonlySet<string>>(noDirty);
  const dirtyRef = useRef(dirtyIds);
  dirtyRef.current = dirtyIds;
  const [state, setState] = useState<TabState>(() =>
    // The current location wins: focus its tab or add one beside the active tab.
    isAuthHref(href)
      ? tabs.load(storage(), sessionKey, Date.now())
      : tabs.land(
          tabs.load(storage(), sessionKey, Date.now()),
          href,
          titleFor(href, page),
          Date.now(),
        ),
  );
  const stateRef = useRef(state);
  const commit = useCallback((next: TabState) => {
    stateRef.current = next;
    setState(next);
  }, []);
  const [confirm, setConfirm] = useState<{
    names: string;
    run: () => void;
  } | null>(null);
  const intent = useRef<{ href: string; how: "replace" | "new" } | null>(null);

  const go = useCallback(
    (to: string, options: { replace?: boolean; ignoreBlocker?: boolean }) => {
      const url = new URL(to, location.origin);
      return router.navigate({
        to: url.pathname,
        search: Object.fromEntries(url.searchParams),
        ...options,
      });
    },
    [router],
  );

  // Router URL = active tab URL. A location nobody asked for (link click,
  // form redirect, back/forward) lands in the tab that already shows it, in
  // the active tab, or in a new tab when the active one is pinned.
  const handled = useRef(href);
  useEffect(() => {
    if (handled.current === href) return;
    handled.current = href;
    if (isAuthHref(href)) return;
    const s = stateRef.current;
    const active = s.tabs.find((t) => t.id === s.activeId);
    if (active && tabs.tabKey(active.href) === tabs.tabKey(href)) return;
    const asked = intent.current;
    intent.current = null;
    const how =
      asked && tabs.tabKey(asked.href) === tabs.tabKey(href)
        ? asked.how
        : "replace";
    commit(
      tabs.open(
        s,
        href,
        titleFor(href, page),
        how,
        Date.now(),
        dirtyRef.current,
      ),
    );
    setMenu(false);
  }, [href, page, commit]);

  // The tab title follows the page it shows.
  useEffect(() => {
    const s = stateRef.current;
    const active = s.tabs.find((t) => t.id === s.activeId);
    if (
      active &&
      tabs.tabKey(active.href) === tabs.tabKey(page.href) &&
      active.title !== page.title
    )
      commit(tabs.rename(s, active.id, page.title));
  }, [page.href, page.title, commit]);

  useEffect(() => {
    tabs.save(storage(), sessionKey, state);
  }, [state, sessionKey]);

  const anyDirty = dirtyIds.size > 0;
  useEffect(() => {
    if (!anyDirty) return;
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [anyDirty]);

  const onDirty = useCallback((id: string, dirty: boolean) => {
    setDirtyIds((prev) => {
      if (prev.has(id) === dirty) return prev;
      const next = new Set(prev);
      if (dirty) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);

  const api = useMemo<TabsApi>(() => {
    // Moving to another tab replaces history and never trips the unsaved-edit
    // guard: the tab stays mounted with its edits.
    const follow = (next: TabState) => {
      const target = next.tabs.find((t) => t.id === next.activeId);
      if (target && target.href !== hrefOf(router.state.location))
        void go(target.href, { replace: true, ignoreBlocker: true });
    };
    // Closing tabs with unsaved edits asks first.
    const closeWith = (ids: string[], run: () => TabState) => {
      const dirty = stateRef.current.tabs.filter(
        (t) => ids.includes(t.id) && dirtyRef.current.has(t.id),
      );
      const apply = () => {
        const next = run();
        commit(next);
        follow(next);
      };
      if (dirty.length === 0) apply();
      else
        setConfirm({ names: dirty.map((t) => t.title).join(", "), run: apply });
    };
    return {
      state,
      dirtyIds,
      openHref(to, how) {
        intent.current = { href: to, how };
        void go(to, { ignoreBlocker: how === "new" });
      },
      activate(id) {
        const next = tabs.activate(stateRef.current, id, Date.now());
        commit(next);
        follow(next);
      },
      close: (id) => closeWith([id], () => tabs.close(stateRef.current, id)),
      closeOthers: (id) =>
        closeWith(
          stateRef.current.tabs
            .filter((t) => t.id !== id && !t.pinned)
            .map((t) => t.id),
          () => tabs.closeOthers(stateRef.current, id),
        ),
      closeRight(id) {
        const all = stateRef.current.tabs;
        closeWith(
          all.slice(all.findIndex((t) => t.id === id) + 1).map((t) => t.id),
          () => tabs.closeRight(stateRef.current, id),
        );
      },
      duplicate(id) {
        const next = tabs.duplicate(stateRef.current, id, Date.now());
        commit(next);
        follow(next);
      },
      reload(id) {
        const tab = stateRef.current.tabs.find((t) => t.id === id);
        if (tab)
          void client.invalidateQueries({ queryKey: queryKey(tab.href) });
      },
      reopen() {
        const next = tabs.reopen(stateRef.current, Date.now());
        commit(next);
        follow(next);
      },
      pin: (id, v) => commit(tabs.setPinned(stateRef.current, id, v)),
      move: (id, i) => commit(tabs.move(stateRef.current, id, i)),
    };
  }, [state, dirtyIds, router, go, commit, client]);

  // One keyboard listener for tab shortcuts: [ and ] step through tabs, g 1..9
  // jump, g w closes, g t reopens. Shifted bindings, when added, must come
  // before unshifted ones on the same key.
  const apiRef = useRef(api);
  apiRef.current = api;
  const openCommandRef = useRef(openCommand);
  openCommandRef.current = openCommand;
  useEffect(() => {
    const now = () => apiRef.current.state;
    const step = (by: number) => {
      const { tabs: list, activeId } = now();
      const at = list.findIndex((t) => t.id === activeId);
      apiRef.current.activate(list[(at + by + list.length) % list.length].id);
    };
    const bindings: Binding[] = [
      { keys: ["BracketLeft"], run: () => step(-1) },
      { keys: ["BracketRight"], run: () => step(1) },
      ...[1, 2, 3, 4, 5, 6, 7, 8, 9].map((n) => ({
        keys: ["KeyG", `Digit${n}`],
        run: () => {
          const tab = now().tabs[n - 1];
          if (tab) apiRef.current.activate(tab.id);
        },
      })),
      {
        keys: ["KeyG", "KeyW"],
        run: () => apiRef.current.close(now().activeId),
      },
      { keys: ["KeyG", "KeyT"], run: () => apiRef.current.reopen() },
    ];
    const match = createMatcher(bindings);
    const onKey = (e: KeyboardEvent) => {
      // The command bar opens from anywhere with Ctrl/Cmd+K, and with / when
      // the user is not typing.
      const plain = !e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey;
      if (
        ((e.metaKey || e.ctrlKey) && e.code === "KeyK") ||
        (plain && e.code === "Slash" && !isTyping(e.target))
      ) {
        e.preventDefault();
        openCommandRef.current();
        return;
      }
      // Menus and dialogs own the keyboard while they are open.
      if (
        (e.target as Element | null)?.closest?.(
          '[role="menu"], [role="dialog"]',
        )
      )
        return;
      if (match(e, Date.now())) e.preventDefault();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const active = state.tabs.find((t) => t.id === state.activeId)!;
  const activeKey = queryKey(active.href);
  const loading = useIsFetching({ queryKey: activeKey }) > 0;
  const refresh = () => void client.refetchQueries({ queryKey: activeKey });
  const name = page.nav.find((n) => n.active)?.label || page.title;

  return (
    <TabsContext.Provider value={api}>
      <div
        data-mode={modeOf(page)}
        className={
          "paymux-react app-shell " + (collapsed ? "sidebar-collapsed" : "")
        }
      >
        <a href="#react-workspace" className="react-skip">
          Skip to workspace
        </a>
        <aside className="react-sidebar" id="desktop-workspace-navigation">
          <Logo />
          <div className="workspace-identity">
            <span className="workspace-initial">P</span>
            <div>
              <strong>Payment workspace</strong>
              <small>Self-hosted infrastructure</small>
            </div>
          </div>
          <Navigation page={page} />
          <div className="sidebar-bottom">
            <span className="ownership-dot" />
            <span>Your infrastructure. Your control.</span>
          </div>
        </aside>
        <div className="react-workspace">
          <RuntimeProvider page={page} guard={false}>
            <header className="react-header">
              <div className="header-location">
                <Sheet open={menu} onOpenChange={setMenu}>
                  <SheetTrigger asChild>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="mobile-workspace-trigger"
                      aria-label="Open workspace navigation"
                    >
                      <Menu size={20} />
                    </Button>
                  </SheetTrigger>
                  <SheetContent side="left" className="mobile-workspace-sheet">
                    <SheetHeader>
                      <SheetTitle>Paymux workspace</SheetTitle>
                    </SheetHeader>
                    <Navigation page={page} close={() => setMenu(false)} />
                  </SheetContent>
                </Sheet>
                <Button
                  variant="ghost"
                  size="icon"
                  className="desktop-sidebar-trigger"
                  aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
                  aria-expanded={!collapsed}
                  aria-controls="desktop-workspace-navigation"
                  onClick={() => {
                    setCollapsed(!collapsed);
                    try {
                      localStorage.setItem(
                        "paymux-sidebar-collapsed",
                        String(!collapsed),
                      );
                    } catch {}
                  }}
                >
                  {collapsed ? (
                    <PanelLeftOpen aria-hidden="true" size={17} />
                  ) : (
                    <PanelLeftClose aria-hidden="true" size={17} />
                  )}
                </Button>
                <span className="breadcrumb-root">Workspace</span>
                <ChevronRight size={14} />
                <strong>{name}</strong>
              </div>
              <div className="header-controls">
                <Badge
                  variant="secondary"
                  className={
                    page.mode.includes("test") ? "test-label" : "live-label"
                  }
                >
                  {page.mode.replace(" mode", "").toUpperCase()}
                </Badge>
                <Button
                  variant="outline"
                  size="sm"
                  className="command-trigger"
                  aria-label="Search and commands"
                  onClick={openCommand}
                >
                  <Search size={14} />
                  <span>Search</span>
                  <kbd>Ctrl K</kbd>
                </Button>
                <RefreshWorkspace
                  refresh={refresh}
                  loading={loading}
                  hasEdits={dirtyIds.has(state.activeId)}
                />
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label={
                    dark ? "Switch to light theme" : "Switch to dark theme"
                  }
                  onClick={toggleTheme}
                >
                  {dark ? <Sun size={17} /> : <Moon size={17} />}
                </Button>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button
                      variant="outline"
                      className="account-trigger"
                      aria-label="Account and mode"
                    >
                      {page.user.slice(0, 2).toUpperCase() || "PM"}
                    </Button>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="end" className="account-menu">
                    <DropdownMenuLabel>{page.user}</DropdownMenuLabel>
                    <DropdownMenuSeparator />
                    {page.modeForm && <RenderNode node={page.modeForm} />}
                    <DropdownMenuSeparator />
                    {page.logoutForm && <RenderNode node={page.logoutForm} />}
                  </DropdownMenuContent>
                </DropdownMenu>
              </div>
            </header>
            <TabStrip iconFor={iconFor} />
            {commandLoaded && (
              <Suspense fallback={null}>
                <CommandBar
                  open={commandOpen}
                  onOpenChange={setCommandOpen}
                  page={page}
                  onToggleTheme={toggleTheme}
                />
              </Suspense>
            )}
          </RuntimeProvider>
          {/* The active tab leads in DOM order so ids, label[for] and
              aria-controls resolve to the visible page. Keyed children move
              without remounting. */}
          {[active, ...state.tabs.filter((t) => t !== active)].map((tab) => (
            <TabView
              key={tab.id}
              tab={tab}
              active={tab.id === state.activeId}
              onDirty={onDirty}
            />
          ))}
          <div className="workspace-bottom">
            <span>Paymux control plane</span>
            <a href="/docs">API reference</a>
          </div>
        </div>
        <Dialog
          open={!!confirm}
          onOpenChange={(open) => {
            if (!open) setConfirm(null);
          }}
        >
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Discard unsaved changes?</DialogTitle>
              <DialogDescription>
                {confirm?.names} has changes that have not been saved. Keep
                editing, or discard them and close.
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="outline" onClick={() => setConfirm(null)}>
                Keep editing
              </Button>
              <Button
                onClick={() => {
                  confirm?.run();
                  setConfirm(null);
                }}
              >
                Discard and close
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
        {slot}
      </div>
    </TabsContext.Provider>
  );
}

export function DashboardApp({ slot }: { slot?: React.ReactNode }) {
  const loc = useLocation();
  const router = useRouter();
  const href = hrefOf(loc);
  // The last page shown keeps the shell mounted while another one loads.
  const shown = useRef<DashboardPage | null>(null);
  const inShell = !!shown.current && !shown.current.auth;
  const { data, error, refetch } = useQuery({
    queryKey: queryKey(href),
    queryFn: ({ signal }) => fetchDashboard(href, signal),
    // Inside the shell the tab views fetch; this observer only reads.
    // Sign-in style pages have no tab, so this observer loads them.
    enabled: !inShell || isAuthHref(href),
  });
  if (data) shown.current = data;
  const page = shown.current;
  const { dark, setDark } = useChrome(page);
  useEffect(() => {
    if (data && data.href !== href) {
      const url = new URL(data.href, window.location.origin);
      router.navigate({
        to: url.pathname,
        search: Object.fromEntries(url.searchParams),
        replace: true,
      });
    }
  }, [data, href, router]);
  if (!page)
    return error ? (
      <div className="workspace-load-error">
        <Alert variant="destructive">
          <AlertTitle>Unable to load workspace</AlertTitle>
          <AlertDescription>{error.message}</AlertDescription>
        </Alert>
        <Button onClick={() => refetch()}>Try again</Button>
        <a href={href + (href.includes("?") ? "&" : "?") + "classic=1"}>
          Use the classic dashboard
        </a>
      </div>
    ) : (
      <div
        className="workspace-loading"
        role="status"
        aria-label="Loading workspace"
      >
        <Skeleton className="h-12 w-56" />
        <Skeleton className="h-80 w-full" />
        <span>Loading workspace…</span>
      </div>
    );
  if (page.auth) return <AuthLayout page={page} />;
  // A different user or mode starts from that session's own tabs.
  const sessionKey = tabs.storageKey(page.user, page.mode);
  return (
    <TabsShell
      slot={slot}
      key={sessionKey}
      page={page}
      sessionKey={sessionKey}
      dark={dark}
      setDark={setDark}
    />
  );
}

export function createDashboardRouter(
  history: RouterHistory = createBrowserHistory(),
  slot?: React.ReactNode,
) {
  const root = createRootRoute({
    component: () => <DashboardApp slot={slot} />,
    validateSearch: (search: Record<string, unknown>) => search,
  });
  const catchAll = createRoute({ getParentRoute: () => root, path: "$" });
  return createRouter({
    routeTree: root.addChildren([catchAll]),
    history,
    defaultPreload: "intent",
    scrollRestoration: true,
  });
}
