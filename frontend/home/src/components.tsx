import { useState, type AnchorHTMLAttributes, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";
import { Check, Copy, LoaderCircle } from "lucide-react";
export function SiteLink({
  href = "",
  exact = false,
  ...props
}: AnchorHTMLAttributes<HTMLAnchorElement> & { exact?: boolean }) {
  if (href.startsWith("/") && !href.startsWith("//")) {
    const url = new URL(href, location.origin);
    return (
      <Link
        to={url.pathname}
        activeOptions={{ exact }}
        search={Object.fromEntries(url.searchParams)}
        hash={url.hash.slice(1)}
        {...props}
      />
    );
  }
  return <a href={href} {...props} />;
}
export function SiteButton({
  children,
  ...props
}: AnchorHTMLAttributes<HTMLAnchorElement>) {
  return (
    <Button asChild size="lg">
      <SiteLink {...props}>{children}</SiteLink>
    </Button>
  );
}
export function CopyButton({ target }: { target: string }) {
  const [status, setStatus] = useState("Copy");
  return (
    <Button
      variant="outline"
      size="sm"
      className="copy"
      aria-live="polite"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(
            document.getElementById(target)?.textContent || "",
          );
          setStatus("Copied");
          setTimeout(() => setStatus("Copy"), 1800);
        } catch {
          setStatus("Select code to copy");
        }
      }}
    >
      {status === "Copied" ? <Check size={14} /> : <Copy size={14} />} {status}
    </Button>
  );
}
