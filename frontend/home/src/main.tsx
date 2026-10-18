import React, { useEffect, useLayoutEffect } from "react";
import { createRoot } from "react-dom/client";
import {
  createRouter,
  createRootRoute,
  createRoute,
  Outlet,
  RouterProvider,
  useLocation,
  Link,
  lazyRouteComponent,
} from "@tanstack/react-router";
import { ArrowUpRight, Menu, Code, Sun, Moon } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";
import { SiteLink } from "./components";
import Home from "./home";
import Integrations from "./integrations";
const Docs = lazyRouteComponent(() => import("./docs"));
import Hosting from "./hosting";
import "../../frontend/styles.css";
import "./marketing.css";
import "./site.css";
const links = [
  ["/integrations", "Integrations"],
  ["/self-hosting", "Self-hosting"],
  ["/docs", "Documentation"],
];
function Layout() {
  const location = useLocation();
  const [open, setOpen] = React.useState(false);
  const [dark, setDark] = React.useState(() => {
    try {
      const saved = localStorage.getItem("paymux-home-theme");
      return saved
        ? saved === "dark"
        : matchMedia("(prefers-color-scheme: dark)").matches;
    } catch {
      return false;
    }
  });
  useLayoutEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    document
      .querySelector('meta[name="theme-color"]')
      ?.setAttribute("content", dark ? "#101c20" : "#fbfcfa");
  }, [dark]);
  function toggleTheme() {
    setDark((value) => {
      try {
        localStorage.setItem("paymux-home-theme", value ? "light" : "dark");
      } catch {
        /* Theme still works when storage is unavailable. */
      }
      return !value;
    });
  }
  useEffect(() => {
    setOpen(false);
    const titles: Record<string, string> = {
      "/": "Payment infrastructure you own",
      "/docs": "Start building",
      "/integrations": "Provider capabilities",
      "/self-hosting": "Self-hosting and security",
    };
    if (!(
      location.pathname === "/docs" || location.pathname.startsWith("/docs/")
    ))
      document.title =
        (titles[location.pathname] || "Page not found") + " · Paymux";
  }, [location.pathname]);
  return (
    <>
      <a className="skip" href="#main">
        Skip to content
      </a>
      <header className="site-head">
        <SiteLink className="logo" href="/" aria-label="Paymux home">
          <span aria-hidden>
            <ArrowUpRight size={25} />
          </span>
          paymux
        </SiteLink>
        <nav className="desktop-site-nav" aria-label="Main">
          {links.map(([href, label]) => (
            <SiteLink
              key={href}
              href={href}
              aria-current={location.pathname === href ? "page" : undefined}
            >
              {label}
            </SiteLink>
          ))}
        </nav>
        <Button asChild variant="ghost" className="github">
          <a href="https://github.com/velixio/paymux">
            <Code size={16} /> GitHub
          </a>
        </Button>
        <Button
          variant="ghost"
          size="icon"
          className="site-theme-toggle"
          onClick={toggleTheme}
          aria-label={dark ? "Use light theme" : "Use dark theme"}
        >
          {dark ? (
            <Sun aria-hidden="true" size={18} />
          ) : (
            <Moon aria-hidden="true" size={18} />
          )}
        </Button>
        <Sheet modal={false} open={open} onOpenChange={setOpen}>
          <SheetTrigger asChild>
            <Button
              variant="outline"
              size="icon"
              className="mobile-site-menu"
              aria-label="Open navigation"
            >
              <Menu />
            </Button>
          </SheetTrigger>
          <SheetContent>
            <SheetHeader>
              <SheetTitle>Explore Paymux</SheetTitle>
            </SheetHeader>
            <nav className="mobile-site-links" aria-label="Mobile navigation">
              {links.map(([href, label]) => (
                <Button key={href} asChild variant="ghost">
                  <SiteLink href={href}>{label}</SiteLink>
                </Button>
              ))}
            </nav>
          </SheetContent>
        </Sheet>
      </header>
      <main id="main">
        <Outlet />
      </main>
      <footer>
        <SiteLink className="logo" href="/">
          paymux
        </SiteLink>
        <p>Payment infrastructure you can inspect, operate and own.</p>
        <div>
          <SiteLink href="/docs">Documentation</SiteLink>
          <a href="https://github.com/velixio/paymux/blob/main/LICENSE">
            Apache 2.0
          </a>
          <a href="https://github.com/velixio/paymux">Source code</a>
        </div>
        <small>Self-hosted. Under active development.</small>
      </footer>
    </>
  );
}
const root = createRootRoute({
  component: Layout,
  notFoundComponent: () => (
    <section className="page-intro">
      <h1>Page not found</h1>
      <p>
        This page does not exist. <Link to="/">Return to Paymux</Link>.
      </p>
    </section>
  ),
});
const routes = [
  createRoute({ getParentRoute: () => root, path: "/", component: Home }),
  createRoute({
    getParentRoute: () => root,
    path: "/integrations",
    component: Integrations,
  }),
  createRoute({ getParentRoute: () => root, path: "/docs", component: Docs }),
  createRoute({
    getParentRoute: () => root,
    path: "/docs/$slug",
    component: Docs,
  }),
  createRoute({
    getParentRoute: () => root,
    path: "/self-hosting",
    component: Hosting,
  }),
];
const router = createRouter({
  routeTree: root.addChildren(routes),
  defaultPreload: "intent",
  scrollRestoration: true,
});
createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <RouterProvider router={router} />
  </React.StrictMode>,
);
