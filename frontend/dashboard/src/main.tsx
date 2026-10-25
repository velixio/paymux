import { setNonce } from "get-nonce";
import React from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import "@fontsource-variable/inter";
import "../../styles.css";
import "./workspace.css";
import "./app-shell.css";
import { parseDashboard } from "./adapter";
import { createDashboardRouter, seedInitialPage } from "./shell";

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
if (initial) seedInitialPage(initialHref, initial);
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
const router = createDashboardRouter();
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
