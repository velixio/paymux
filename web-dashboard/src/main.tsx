import { setNonce } from "get-nonce";
import React, { useEffect, useState, useRef } from "react";
import { createRoot } from "react-dom/client";
import {
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
  useLocation,
  useRouter,
} from "@tanstack/react-router";
import {
  QueryClient,
  QueryClientProvider,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
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
import { fetchDashboard, parseDashboard, type DashboardPage } from "./adapter";
import {
  RuntimeProvider,
  WorkspaceLink,
  queryKey,
  useRuntime,
} from "./runtime";
import { PageContent, RenderNode } from "./render";
import "../../frontend/styles.css";
import "./workspace.css";
import "./app-shell.css";
const source = document.getElementById("paymux-server-page");
const mount = document.getElementById("paymux-react-root");
if (mount?.dataset.styleNonce) setNonce(mount.dataset.styleNonce);
const initialHref = location.pathname + location.search;
let initial = source ? parseDashboard(document, initialHref) : null;
const client = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      gcTime: 0,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});
if (initial) client.setQueryData(queryKey(initialHref), initial);
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
}: {
  refresh: () => void;
  loading: boolean;
}) {
  const { hasEdits } = useRuntime();
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
function Workspace({
  page,
  refresh,
  loading,
}: {
  page: DashboardPage;
  refresh: () => void;
  loading: boolean;
}) {
  const [menu, setMenu] = useState(false);
  const [collapsed, setCollapsed] = useState(() => {
    try {
      return localStorage.getItem("paymux-sidebar-collapsed") === "true";
    } catch {
      return false;
    }
  });
  const contentRef = useRef<HTMLElement>(null);
  const loc = useLocation();
  const [dark, setDark] = useState(() => {
    try {
      return (
        localStorage.getItem("paymux-theme") === "dark" ||
        (!localStorage.getItem("paymux-theme") &&
          (page.theme?.includes("/minimal-dark/") ||
            matchMedia("(prefers-color-scheme:dark)").matches))
      );
    } catch {
      return false;
    }
  });
  useEffect(() => {
    setMenu(false);
    document.title = page.title + " - Paymux";
    if (source) {
      source.textContent = "";
      source.remove();
    }
    mount?.setAttribute("data-ready", "true");
  }, [loc.pathname, page.title]);
  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
  }, [dark]);
  useEffect(() => {
    let link = document.querySelector<HTMLLinkElement>('link[href^="/theme/"]');
    if (page.theme) {
      if (!link) {
        link = document.createElement("link");
        link.rel = "stylesheet";
        document.head.append(link);
      }
      link.href = page.theme;
    } else link?.remove();
    try {
      if (!localStorage.getItem("paymux-theme"))
        setDark(
          !!page.theme?.includes("/minimal-dark/") ||
            matchMedia("(prefers-color-scheme:dark)").matches,
        );
    } catch {}
  }, [page.theme]);
  useEffect(() => {
    contentRef.current?.scrollTo({ top: 0, left: 0, behavior: "auto" });
  }, [loc.pathname, loc.searchStr]);
  const name = page.nav.find((n) => n.active)?.label || page.title;
  return (
    <RuntimeProvider page={page}>
      <div
        className={
          "paymux-react " +
          (page.auth
            ? "auth-workspace"
            : "app-shell " + (collapsed ? "sidebar-collapsed" : ""))
        }
      >
        <a href="#react-workspace" className="react-skip">
          Skip to workspace
        </a>
        {page.auth ? (
          <div className="auth-container">
            <Logo />
            <div className="auth-card">
              <PageContent key={page.href} nodes={page.content} />
            </div>
            <p className="auth-footnote">Self-hosted payment infrastructure</p>
          </div>
        ) : (
          <>
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
                    <SheetContent
                      side="left"
                      className="mobile-workspace-sheet"
                    >
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
                    aria-label={
                      collapsed ? "Expand sidebar" : "Collapse sidebar"
                    }
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
                    {page.mode.replace(" mode", "")}
                  </Badge>
                  <RefreshWorkspace refresh={refresh} loading={loading} />
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={dark ? "Use light theme" : "Use dark theme"}
                    onClick={() => {
                      setDark(!dark);
                      try {
                        localStorage.setItem(
                          "paymux-theme",
                          !dark ? "dark" : "light",
                        );
                      } catch {}
                    }}
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
              {page.warnings.map((message, i) => (
                <div className="workspace-warning" role="status" key={i}>
                  {message}
                </div>
              ))}
              <section
                className="react-content"
                ref={contentRef}
                id="react-workspace"
                tabIndex={-1}
              >
                <PageContent
                  key={
                    page.href +
                    "-" +
                    page.content.map((n) => n.attrs.id || n.tag).join("")
                  }
                  nodes={page.content}
                />
              </section>
              <div className="workspace-bottom">
                <span>Paymux control plane</span>
                <a href="/docs">API reference</a>
              </div>
            </div>
          </>
        )}
      </div>
    </RuntimeProvider>
  );
}
function DashboardApp() {
  const location = useLocation();
  const router = useRouter();
  const href = location.pathname + location.searchStr;
  const { data, error, isFetching, refetch, dataUpdatedAt } = useQuery({
    queryKey: queryKey(href),
    queryFn: ({ signal }) => fetchDashboard(href, signal),
  });
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
  if (error && !data)
    return (
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
    );
  if (!data)
    return (
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
  return (
    <Workspace
      key={dataUpdatedAt}
      page={data}
      refresh={() => refetch()}
      loading={isFetching}
    />
  );
}
class Boundary extends React.Component<
  { children: React.ReactNode },
  { failed: boolean }
> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  render() {
    return this.state.failed ? (
      <div className="workspace-load-error">
        <h1>The workspace could not be displayed</h1>
        <p>Reload the workspace or use the classic dashboard to continue.</p>
        <Button asChild>
          <a
            href={
              location.pathname +
              location.search +
              (location.search ? "&" : "?") +
              "classic=1"
            }
          >
            Open classic dashboard
          </a>
        </Button>
      </div>
    ) : (
      this.props.children
    );
  }
}
const root = createRootRoute({
  component: DashboardApp,
  validateSearch: (search: Record<string, unknown>) => search,
});
const catchAll = createRoute({ getParentRoute: () => root, path: "$" });
const router = createRouter({
  routeTree: root.addChildren([catchAll]),
  defaultPreload: "intent",
  scrollRestoration: true,
});
if (mount) {
  if (new URLSearchParams(location.search).has("classic")) {
    mount.remove();
    const script = document.createElement("script");
    script.src = "/assets/js/workspace.js";
    document.body.append(script);
  } else
    createRoot(mount).render(
      <Boundary>
        <QueryClientProvider client={client}>
          <RouterProvider router={router} />
        </QueryClientProvider>
      </Boundary>,
    );
}

initial = null;
