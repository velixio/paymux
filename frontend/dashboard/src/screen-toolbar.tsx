import { useMemo, useRef } from "react";
import { useRouter } from "@tanstack/react-router";
import { ArrowLeft, Info } from "lucide-react";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import type { ViewNode } from "./adapter";
import { PageContent, RenderNode } from "./render";
import { WorkspaceLink } from "./runtime";
import { extractToolbar, type ToolbarModel } from "./toolbar";
import { useEdgeFade } from "./use-edge-fade";
import { statusLabel, statusTone } from "./pill";

function StatusChips({
  options,
}: {
  options: NonNullable<ToolbarModel["statuses"]>["options"];
}) {
  const router = useRouter();
  // Start from the tab's own query so other filters survive the change.
  function apply(value: string) {
    const here = router.state.location;
    const url = new URL(here.pathname + here.searchStr, location.origin);
    if (value) url.searchParams.set("status", value);
    else url.searchParams.delete("status");
    void router.navigate({
      to: url.pathname,
      search: Object.fromEntries(url.searchParams),
    });
  }
  // Fade the edges that hide chips, and bring the selected chip into view.
  const row = useRef<HTMLDivElement>(null);
  useEdgeFade(row, '[aria-pressed="true"]', [options]);
  return (
    <div ref={row} role="group" aria-label="Status" className="toolbar-chips">
      {options.map((o) => (
        <button
          type="button"
          key={o.value || "all"}
          className="toolbar-chip"
          aria-pressed={o.selected}
          onClick={() => apply(o.value)}
        >
          {o.value && (
            <span
              className={`pill-dot tone-${statusTone(o.value)}`}
              aria-hidden="true"
            />
          )}
          {o.value ? statusLabel(o.label) : o.label}
        </button>
      ))}
    </div>
  );
}

export function ScreenToolbar({ model }: { model: ToolbarModel }) {
  return (
    <div className="screen-toolbar">
      {model.back && (
        <WorkspaceLink
          className="toolbar-back"
          href={model.back.href}
          aria-label={`Back to ${model.back.label}`}
          title={`Back to ${model.back.label}`}
        >
          <ArrowLeft size={15} />
        </WorkspaceLink>
      )}
      <h1 className="toolbar-title">
        {model.title.map((n, i) => (
          <RenderNode key={i} node={n} />
        ))}
      </h1>
      {model.description && (
        <Popover>
          <PopoverTrigger asChild>
            <button
              type="button"
              className="toolbar-info"
              aria-label="About this page"
            >
              <Info size={14} />
            </button>
          </PopoverTrigger>
          <PopoverContent align="start" className="toolbar-description">
            <p>{model.description}</p>
          </PopoverContent>
        </Popover>
      )}
      {model.statuses && <StatusChips options={model.statuses.options} />}
      {model.actions.length > 0 && (
        <div className="toolbar-actions">
          {model.actions.map((n: ViewNode, i) => (
            <RenderNode key={i} node={n} />
          ))}
        </div>
      )}
    </div>
  );
}

// A screen's content: the toolbar built from the server markup, then the rest.
// The toolbar is taken out first; the rest may still split into inner
// section tabs (record pages), so href/syncHash pass through.
export function ScreenContent({
  nodes,
  href,
  syncHash,
}: {
  nodes: ViewNode[];
  href?: string;
  syncHash?: boolean;
}) {
  const parts = useMemo(() => extractToolbar(nodes), [nodes]);
  if (!parts)
    return <PageContent nodes={nodes} href={href} syncHash={syncHash} />;
  return (
    <>
      <ScreenToolbar model={parts.toolbar} />
      <PageContent nodes={parts.rest} href={href} syncHash={syncHash} />
    </>
  );
}
