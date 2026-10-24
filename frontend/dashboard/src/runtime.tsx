import React, {
  createContext,
  useContext,
  useCallback,
  useEffect,
  useState,
  type ReactNode,
} from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useRouter, Link, useBlocker } from "@tanstack/react-router";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { LoaderCircle } from "lucide-react";
import {
  parseDashboard,
  isDownloadPath,
  preservePostedValues,
  type DashboardPage,
} from "./adapter";
export const queryKey = (href: string) => ["dashboard", href];
type Submission = {
  form: HTMLFormElement;
  button: HTMLButtonElement | null;
  data: FormData;
  action: string;
  method: string;
  label: string;
};
type Runtime = {
  page: DashboardPage;
  submit: (form: HTMLFormElement, button: HTMLButtonElement | null) => void;
  pending: boolean;
  markDirty: (form: HTMLFormElement, dirty: boolean) => void;
  hasEdits: boolean;
  copy: (value: string) => Promise<void>;
};
const Context = createContext<Runtime | null>(null);
export function useRuntime() {
  const value = useContext(Context);
  if (!value) throw Error("Missing workspace runtime");
  return value;
}
export function WorkspaceLink({
  href = "",
  children,
  ...props
}: React.AnchorHTMLAttributes<HTMLAnchorElement>) {
  const url = new URL(href, location.href);
  return !props.target &&
    !props.download &&
    !isDownloadPath(url.pathname) &&
    url.origin === location.origin &&
    (/^\/dashboard(?:\/|$)/.test(url.pathname) || url.pathname === "/setup") ? (
    <Link
      to={url.pathname}
      search={Object.fromEntries(url.searchParams)}
      hash={url.hash.slice(1)}
      {...props}
      onClick={(event) => {
        if (["#routing", "#route-test"].includes(url.hash)) {
          const details = document.querySelector<HTMLDetailsElement>(
            ".react-content .routing-disclosure",
          );
          if (details) details.open = true;
        }
        props.onClick?.(event);
      }}
    >
      {children}
    </Link>
  ) : (
    <a href={href} {...props}>
      {children}
    </a>
  );
}
export function RuntimeProvider({
  page,
  children,
  active = true,
  onDirtyChange,
}: {
  page: DashboardPage;
  children: ReactNode;
  /** Only the visible tab guards navigation; hidden tabs keep their edits. */
  active?: boolean;
  /** Reports whether this view has unsaved edits (false again on unmount). */
  onDirtyChange?: (dirty: boolean) => void;
}) {
  const router = useRouter();
  const client = useQueryClient();
  const [review, setReview] = useState<Submission | null>(null);
  const [notice, setNotice] = useState("");
  const [dirtyForms, setDirtyForms] = useState(new Set<HTMLFormElement>());
  const markDirty = useCallback(
    (form: HTMLFormElement, dirty: boolean) =>
      setDirtyForms((previous) => {
        const next = new Set(previous);
        if (dirty) next.add(form);
        else next.delete(form);
        return next;
      }),
    [],
  );
  const dirty = dirtyForms.size > 0;
  useEffect(() => {
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);
  useEffect(() => () => onDirtyChange?.(false), [onDirtyChange]);
  const mutation = useMutation({
    mutationFn: async (s: Submission) => {
      const hasFiles = [...s.data.values()].some(
        (v) => v instanceof File && v.size > 0,
      );
      const payload =
        hasFiles || s.form.enctype === "multipart/form-data"
          ? s.data
          : new URLSearchParams(
              [...s.data.entries()].map(([k, v]) => [
                k,
                typeof v === "string" ? v : v.name,
              ]),
            );
      const response = await fetch(s.action, {
        method: s.method,
        body: payload,
        credentials: "same-origin",
        headers: { Accept: "text/html" },
      });
      if (!response.headers.get("content-type")?.includes("text/html")) {
        if (!response.ok)
          throw Error(`Unable to complete this action (${response.status}).`);
        const url = URL.createObjectURL(await response.blob());
        const link = document.createElement("a");
        link.href = url;
        link.download =
          response.headers
            .get("content-disposition")
            ?.match(/filename="?([^";]+)"?/)?.[1] || "paymux-export";
        link.click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
        return null;
      }
      const doc = new DOMParser().parseFromString(
        await response.text(),
        "text/html",
      );
      if (!doc.getElementById("paymux-server-page")) {
        location.assign(response.url);
        return null;
      }
      const href = response.redirected
        ? new URL(response.url).pathname + new URL(response.url).search
        : page.href;
      const next = parseDashboard(doc, href);
      next.focusSetting = s.form.closest("tr[id^=setting-]")?.id;
      return response.ok ? next : preservePostedValues(next, s.action, s.data);
    },
    onSuccess: async (next) => {
      if (!next) return;
      if ((next.auth && !page.auth) || next.mode !== page.mode) client.clear();
      await client.invalidateQueries({
        queryKey: ["dashboard"],
        refetchType: "none",
      });
      client.setQueryData(queryKey(next.href), next);
      const url = new URL(next.href, location.origin);
      await router.navigate({
        to: url.pathname,
        search: Object.fromEntries(url.searchParams),
        ignoreBlocker: true,
      });
      setNotice("");
    },
    onError: () => setNotice(""),
  });
  const blocker = useBlocker({
    shouldBlockFn: ({ current, next }) =>
      (current.pathname !== next.pathname ||
        JSON.stringify(current.search) !== JSON.stringify(next.search)) &&
      (dirtyForms.size > 0 || mutation.isPending),
    // The shell arms one beforeunload listener for every tab.
    enableBeforeUnload: false,
    disabled: !active,
    withResolver: true,
  });
  function submit(form: HTMLFormElement, button: HTMLButtonElement | null) {
    if (mutation.isPending) return;
    const action = new URL(
      button?.getAttribute("formaction") ||
        form.getAttribute("action") ||
        page.href,
      location.origin,
    );
    if (action.origin !== location.origin) {
      form.submit();
      return;
    }
    const data = new FormData(form);
    if (button?.name) data.append(button.name, button.value);
    const method = (
      button?.getAttribute("formmethod") ||
      form.method ||
      "get"
    ).toUpperCase();
    if (method === "GET") {
      action.search = new URLSearchParams(
        [...data.entries()].map(([k, v]) => [
          k,
          typeof v === "string" ? v : v.name,
        ]),
      ).toString();
      router.navigate({
        to: action.pathname,
        search: Object.fromEntries(action.searchParams),
        ignoreBlocker: true,
      });
      return;
    }
    const submission = {
      form,
      button,
      data,
      action: action.href,
      method,
      label: button?.textContent?.trim() || "Save changes",
    };
    const needsReview =
      form.classList.contains("setting-form") ||
      /\/(refund|revoke|rotate|accept|submit|evidence|replay|reissue|disable)$/.test(
        action.pathname,
      ) ||
      /\/settings(?:\/|$)/.test(action.pathname);
    if (needsReview) setReview(submission);
    else mutation.mutate(submission);
  }
  async function copy(value: string) {
    try {
      await navigator.clipboard.writeText(value);
      setNotice("Copied to clipboard.");
      setTimeout(() => setNotice(""), 2000);
    } catch {
      setNotice("Copy unavailable. Select the value and copy it manually.");
    }
  }
  const reviewValue = review?.data.getAll("value").at(-1);
  const reviewAmount = review?.data.get("amount");
  return (
    <Context.Provider
      value={{
        page,
        submit,
        pending: mutation.isPending,
        copy,
        markDirty,
        hasEdits: dirtyForms.size > 0,
      }}
    >
      {mutation.isPending && (
        <div className="workspace-saving" role="status">
          <LoaderCircle size={15} className="refreshing" /> Saving changes…
        </div>
      )}
      <Dialog
        open={blocker.status === "blocked"}
        onOpenChange={(open) => {
          if (!open) blocker.reset?.();
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {mutation.isPending
                ? "An action is in progress"
                : "Discard unsaved changes?"}
            </DialogTitle>
            <DialogDescription>
              {mutation.isPending
                ? "Wait for the current action to finish before leaving this page."
                : "Your changes have not been saved. Stay here to finish editing, or discard them and leave this page."}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => blocker.reset?.()}>
              Keep editing
            </Button>
            {!mutation.isPending && (
              <Button onClick={() => blocker.proceed?.()}>
                Discard and leave
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>
      {notice && (
        <div className="workspace-toast" role="status">
          {notice}
        </div>
      )}
      {mutation.isError && (
        <Alert variant="destructive" className="mutation-error">
          <AlertTitle>Unable to save</AlertTitle>
          <AlertDescription>
            {mutation.error.message} Your edits are still available. Try again.
          </AlertDescription>
          <Button variant="outline" onClick={() => mutation.reset()}>
            Dismiss
          </Button>
        </Alert>
      )}
      {children}
      <Dialog
        open={!!review}
        onOpenChange={(open) => {
          if (!open) {
            review?.button?.focus();
            setReview(null);
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Review change</DialogTitle>
            <DialogDescription>
              Check the scope and value before applying this change. Financial
              actions use the existing Paymux server permissions and validation.
            </DialogDescription>
          </DialogHeader>
          <dl className="review-summary">
            <dt>Action</dt>
            <dd>{review?.label}</dd>
            <dt>Scope</dt>
            <dd>
              {review?.action.includes("/apps/")
                ? "Selected app"
                : review?.action.includes("/settings")
                  ? "Organization · apps without an override"
                  : "Selected record"}
            </dd>
            {reviewValue !== undefined && (
              <>
                <dt>New value</dt>
                <dd>{String(reviewValue)}</dd>
              </>
            )}
            {review?.action.endsWith("/refund") && (
              <>
                <dt>Refund amount</dt>
                <dd>
                  {String(reviewAmount || "All remaining refundable funds")}
                </dd>
              </>
            )}
            {review?.action.endsWith("/reset") && (
              <>
                <dt>Result</dt>
                <dd>
                  Remove this app override and inherit the organization value.
                </dd>
              </>
            )}
          </dl>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => {
                review?.button?.focus();
                setReview(null);
              }}
            >
              Cancel
            </Button>
            <Button
              onClick={() => {
                if (review) {
                  mutation.mutate(review);
                  setReview(null);
                }
              }}
            >
              Confirm change
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Context.Provider>
  );
}
